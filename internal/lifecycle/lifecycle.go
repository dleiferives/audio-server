package lifecycle

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type Manager struct {
	BinaryPath string
	startMu    sync.Mutex
	mu         sync.Mutex
	instances  map[string]*instance
}

type instance struct {
	name           string
	binaryPath     string
	args           []string
	healthURL      string
	idleTimeout    time.Duration
	watcherStarted bool

	mu       sync.Mutex
	cmd      *exec.Cmd
	lastUsed time.Time
	stopCh   chan struct{}
}

func NewManager(binaryPath string) *Manager {
	return &Manager{
		BinaryPath: binaryPath,
		instances:  make(map[string]*instance),
	}
}

func (m *Manager) Register(name, configPath string, port int, idleTimeout time.Duration) {
	m.RegisterCommand(
		name,
		m.BinaryPath,
		[]string{"--config", configPath},
		fmt.Sprintf("http://127.0.0.1:%d/health", port),
		idleTimeout,
	)
}

// RegisterCommand registers an arbitrary sidecar process in the shared
// lifecycle group. Starting it exclusively stops every other registered
// process first, which lets native and Python GPU providers share VRAM.
func (m *Manager) RegisterCommand(name, binaryPath string, args []string, healthURL string, idleTimeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[name] = &instance{
		name:        name,
		binaryPath:  binaryPath,
		args:        append([]string(nil), args...),
		healthURL:   healthURL,
		idleTimeout: idleTimeout,
		stopCh:      make(chan struct{}),
	}
}

// Start starts the named instance. If exclusive is true, all other running
// instances are stopped first (for shared GPU VRAM scenarios).
func (m *Manager) Start(name string, exclusive bool) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	m.mu.Lock()
	inst, ok := m.instances[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("lifecycle: unknown instance %q", name)
	}

	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.cmd != nil && inst.cmd.Process != nil {
		inst.lastUsed = time.Now()
		return nil
	}

	if exclusive {
		m.stopOthersLocked(name)
	}

	log.Printf("lifecycle: starting %s", inst.name)
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

	if inst.idleTimeout > 0 && !inst.watcherStarted {
		inst.watcherStarted = true
		go m.idleWatcher(inst)
	}
	return nil
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
		other.stopLocked()
		other.mu.Unlock()
	}
}

func (m *Manager) StopAll() {
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
			inst.mu.Lock()
			if inst.cmd != nil && time.Since(inst.lastUsed) > inst.idleTimeout {
				inst.stopLocked()
			}
			inst.mu.Unlock()
		}
	}
}
