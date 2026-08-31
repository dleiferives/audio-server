package lifecycle

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Manager struct {
	BinaryPath string
	startMu    sync.Mutex
	execMu     sync.Mutex
	mu         sync.Mutex
	instances  map[string]*instance
	maxVRAMMiB int
}

type instance struct {
	name           string
	binaryPath     string
	args           []string
	healthURL      string
	idleTimeout    time.Duration
	vramMiB        int
	watcherStarted bool

	mu       sync.Mutex
	cmd      *exec.Cmd
	lastUsed time.Time
	active   int
	stopCh   chan struct{}
}

type Config struct {
	// MaxVRAMMiB is the residency budget. Zero disables budget-based eviction.
	MaxVRAMMiB int
}

func NewManager(binaryPath string, configs ...Config) *Manager {
	var cfg Config
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return &Manager{
		BinaryPath: binaryPath,
		instances:  make(map[string]*instance),
		maxVRAMMiB: cfg.MaxVRAMMiB,
	}
}

// DetectVRAMMiB returns the first NVIDIA GPU's total memory without requiring
// the Go process itself to link against CUDA.
func DetectVRAMMiB() (int, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, fmt.Errorf("detect VRAM: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	value, err := strconv.Atoi(line)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("detect VRAM: invalid nvidia-smi output %q", line)
	}
	return value, nil
}

func (m *Manager) Register(name, configPath string, port int, idleTimeout time.Duration) {
	m.RegisterModel(name, configPath, port, idleTimeout, 0)
}

func (m *Manager) RegisterModel(name, configPath string, port int, idleTimeout time.Duration, vramMiB int) {
	m.RegisterCommandModel(
		name,
		m.BinaryPath,
		[]string{"--config", configPath},
		fmt.Sprintf("http://127.0.0.1:%d/health", port),
		idleTimeout,
		vramMiB,
	)
}

// RegisterCommand registers an arbitrary sidecar process in the shared
// lifecycle group. Starting it exclusively stops every other registered
// process first, which lets native and Python GPU providers share VRAM.
func (m *Manager) RegisterCommand(name, binaryPath string, args []string, healthURL string, idleTimeout time.Duration) {
	m.RegisterCommandModel(name, binaryPath, args, healthURL, idleTimeout, 0)
}

func (m *Manager) RegisterCommandModel(name, binaryPath string, args []string, healthURL string, idleTimeout time.Duration, vramMiB int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[name] = &instance{
		name:        name,
		binaryPath:  binaryPath,
		args:        append([]string(nil), args...),
		healthURL:   healthURL,
		idleTimeout: idleTimeout,
		vramMiB:     max(vramMiB, 0),
		stopCh:      make(chan struct{}),
	}
}

func (m *Manager) Has(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.instances[name]
	return ok
}

// Acquire serializes GPU execution and protects the selected model from
// eviction until the returned release function is called.
func (m *Manager) Acquire(name string) (func(), error) {
	m.execMu.Lock()
	if err := m.start(name, false, true); err != nil {
		m.execMu.Unlock()
		return nil, err
	}
	return func() {
		m.mu.Lock()
		inst := m.instances[name]
		m.mu.Unlock()
		if inst != nil {
			inst.mu.Lock()
			if inst.active > 0 {
				inst.active--
			}
			inst.lastUsed = time.Now()
			inst.mu.Unlock()
		}
		m.execMu.Unlock()
	}, nil
}

// Start ensures the named instance is resident. Exclusive is retained for
// callers that explicitly require the old one-model-only behavior.
func (m *Manager) Start(name string, exclusive bool) error {
	return m.start(name, exclusive, false)
}

func (m *Manager) start(name string, exclusive, acquire bool) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	m.mu.Lock()
	inst, ok := m.instances[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("lifecycle: unknown instance %q", name)
	}

	inst.mu.Lock()
	if inst.cmd != nil && inst.cmd.Process != nil {
		inst.lastUsed = time.Now()
		if acquire {
			inst.active++
		}
		inst.mu.Unlock()
		return nil
	}
	inst.mu.Unlock()

	if exclusive {
		m.stopOthersLocked(name)
	} else if err := m.makeRoomLocked(name, inst.vramMiB); err != nil {
		return err
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()
	log.Printf("lifecycle: starting %s (estimated VRAM %d MiB)", inst.name, inst.vramMiB)
	cmd := exec.Command(inst.binaryPath, inst.args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lifecycle %s start: %w", inst.name, err)
	}
	inst.cmd = cmd
	inst.lastUsed = time.Now()

	// Wait for server to become healthy
	deadline := time.Now().Add(30 * time.Second)
	healthClient := &http.Client{Timeout: 2 * time.Second}
	healthy := false
	for time.Now().Before(deadline) {
		resp, err := healthClient.Get(inst.healthURL)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			healthy = true
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !healthy {
		inst.stopLocked()
		return fmt.Errorf("lifecycle %s start: health check timed out", inst.name)
	}
	if acquire {
		inst.active++
	}

	if inst.idleTimeout > 0 && !inst.watcherStarted {
		inst.watcherStarted = true
		go m.idleWatcher(inst)
	}
	return nil
}

// makeRoomLocked evicts idle resident models in least-recently-used order.
// m.startMu must be held so residency cannot change during the calculation.
func (m *Manager) makeRoomLocked(requested string, requiredMiB int) error {
	if m.maxVRAMMiB <= 0 || requiredMiB <= 0 {
		return nil
	}
	if requiredMiB > m.maxVRAMMiB {
		return fmt.Errorf("lifecycle: %s requires an estimated %d MiB VRAM, over the %d MiB limit", requested, requiredMiB, m.maxVRAMMiB)
	}

	type resident struct {
		inst *instance
		used int
		last time.Time
	}
	var residents []resident
	used := 0
	m.mu.Lock()
	for name, candidate := range m.instances {
		if name == requested {
			continue
		}
		candidate.mu.Lock()
		if candidate.cmd != nil && candidate.cmd.Process != nil {
			used += candidate.vramMiB
			residents = append(residents, resident{inst: candidate, used: candidate.vramMiB, last: candidate.lastUsed})
		}
		candidate.mu.Unlock()
	}
	m.mu.Unlock()
	if used+requiredMiB <= m.maxVRAMMiB {
		return nil
	}

	sort.Slice(residents, func(i, j int) bool { return residents[i].last.Before(residents[j].last) })
	for _, candidate := range residents {
		candidate.inst.mu.Lock()
		if candidate.inst.active == 0 && candidate.inst.cmd != nil {
			candidate.inst.stopLocked()
			used -= candidate.used
		}
		candidate.inst.mu.Unlock()
		if used+requiredMiB <= m.maxVRAMMiB {
			return nil
		}
	}
	return fmt.Errorf("lifecycle: insufficient idle VRAM for %s: need %d MiB within %d MiB limit", requested, requiredMiB, m.maxVRAMMiB)
}

func (m *Manager) Bump(name string) {
	m.mu.Lock()
	inst, ok := m.instances[name]
	m.mu.Unlock()
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.lastUsed = time.Now()
	inst.mu.Unlock()
}

func (m *Manager) stopOthersLocked(exclude string) {
	for name, other := range m.instances {
		if name == exclude {
			continue
		}
		other.mu.Lock()
		if other.active == 0 {
			other.stopLocked()
		}
		other.mu.Unlock()
	}
}

func (m *Manager) StopAll() {
	m.execMu.Lock()
	defer m.execMu.Unlock()
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		inst.mu.Lock()
		inst.stopLocked()
		close(inst.stopCh)
		inst.mu.Unlock()
	}
}

func (inst *instance) stopLocked() {
	if inst.cmd == nil || inst.cmd.Process == nil {
		return
	}
	log.Printf("lifecycle: stopping %s", inst.name)
	pgid, _ := syscall.Getpgid(inst.cmd.Process.Pid)
	syscall.Kill(-pgid, syscall.SIGTERM)
	inst.cmd.Wait()
	inst.cmd = nil
}

func (m *Manager) idleWatcher(inst *instance) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-inst.stopCh:
			return
		case <-ticker.C:
			m.startMu.Lock()
			inst.mu.Lock()
			if inst.cmd != nil && inst.active == 0 && time.Since(inst.lastUsed) > inst.idleTimeout {
				inst.stopLocked()
			}
			inst.mu.Unlock()
			m.startMu.Unlock()
		}
	}
}
