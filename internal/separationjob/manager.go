// Package separationjob runs asynchronous stem-separation jobs and persists
// their output stems for later retrieval, unlike the analysis job's internal
// use of a separator (which consumes the dialogue stem and discards the
// rest).
package separationjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
	"github.com/dleiferives/audio-server/internal/sttprovider"
)

var ErrNotFound = errors.New("separation job not found")

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Request struct {
	Audio           []byte
	Filename        string
	SeparationModel string
	// Stems optionally filters which of the provider's produced stems to
	// keep; empty means keep everything the provider returns.
	Stems []string
}

type Result struct {
	ProviderID string   `json:"separation_model"`
	Stems      []string `json:"stems"`
}

type Job struct {
	ID         string
	Status     Status
	Result     Result
	Err        error
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// StemStore persists stem audio, keyed by an opaque string built from the
// job ID and stem name. *store.Store already satisfies this.
type StemStore interface {
	Write(key string, audio []byte) error
	Read(key string) ([]byte, error)
}

type Config struct {
	Separators       map[string]analysisprovider.Separator
	DefaultSeparator string
	AudioNormalizer  sttprovider.AudioNormalizer
	Store            StemStore
	RunGate          func(providerID string) (func(), error)
	Timeout          time.Duration
}

type Manager struct {
	mu               sync.RWMutex
	jobs             map[string]*Job
	separators       map[string]analysisprovider.Separator
	defaultSeparator string
	audioNormalizer  sttprovider.AudioNormalizer
	store            StemStore
	runGate          func(string) (func(), error)
	timeout          time.Duration
}

func New(cfg Config) *Manager {
	return &Manager{
		jobs: make(map[string]*Job), separators: cfg.Separators, defaultSeparator: cfg.DefaultSeparator,
		audioNormalizer: cfg.AudioNormalizer, store: cfg.Store,
		runGate: cfg.RunGate, timeout: cfg.Timeout,
	}
}

func (m *Manager) Enabled() bool { return m != nil && len(m.separators) > 0 && m.store != nil }

// SeparatorIDs reports every selectable separation model id.
func (m *Manager) SeparatorIDs() []string {
	if m == nil {
		return nil
	}
	ids := make([]string, 0, len(m.separators))
	for id := range m.separators {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) Submit(req Request) (*Job, error) {
	if !m.Enabled() {
		return nil, errors.New("audio separation is not configured")
	}
	if len(req.Audio) == 0 {
		return nil, errors.New("audio is required")
	}
	model := req.SeparationModel
	if model == "" || model == "auto" {
		model = m.defaultSeparator
	}
	if _, ok := m.separators[model]; !ok {
		return nil, fmt.Errorf("unknown separation model %q", req.SeparationModel)
	}
	req.SeparationModel = model
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job := &Job{ID: id, Status: StatusQueued, CreatedAt: time.Now()}
	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()
	go m.run(job, req)
	return clone(job), nil
}

func (m *Manager) Get(id string) (*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(job), nil
}

// Stem returns one previously-persisted stem's audio for a succeeded job.
func (m *Manager) Stem(id, name string) ([]byte, error) {
	job, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if job.Status != StatusSucceeded {
		return nil, fmt.Errorf("separation job %s is not finished", id)
	}
	found := false
	for _, s := range job.Result.Stems {
		if s == name {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("separation job %s has no stem %q", id, name)
	}
	return m.store.Read(stemKey(id, name))
}

func (m *Manager) run(job *Job, req Request) {
	m.mu.Lock()
	job.Status = StatusRunning
	job.StartedAt = time.Now()
	m.mu.Unlock()

	ctx := context.Background()
	var cancel context.CancelFunc
	if m.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
	result, err := m.separate(ctx, job.ID, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	job.FinishedAt = time.Now()
	job.Result = result
	job.Err = err
	if err != nil {
		job.Status = StatusFailed
	} else {
		job.Status = StatusSucceeded
	}
}

func (m *Manager) separate(ctx context.Context, jobID string, req Request) (Result, error) {
	if m.audioNormalizer == nil {
		return Result{}, errors.New("audio normalization is not configured")
	}
	separator, ok := m.separators[req.SeparationModel]
	if !ok {
		return Result{}, fmt.Errorf("unknown separation model %q", req.SeparationModel)
	}
	normalized, err := m.audioNormalizer.NormalizeAudio(ctx, req.Audio, sttprovider.AudioFormat{
		Container: "wav", Codec: "pcm_s16le", SampleRate: 44100, Channels: 2,
	})
	if err != nil {
		return Result{}, fmt.Errorf("normalize separation input: %w", err)
	}
	var release func()
	if m.runGate != nil {
		release, err = m.runGate(separator.ID())
		if err != nil {
			return Result{}, fmt.Errorf("acquire separation model: %w", err)
		}
		defer release()
	}
	separated, err := separator.Separate(ctx, analysisprovider.AudioRequest{Audio: normalized.Audio, Filename: req.Filename})
	if err != nil {
		return Result{}, err
	}

	wanted := make(map[string]bool, len(req.Stems))
	for _, s := range req.Stems {
		wanted[strings.ToLower(strings.TrimSpace(s))] = true
	}
	var stems []string
	for name, audio := range separated.Outputs {
		if len(wanted) > 0 && !wanted[strings.ToLower(name)] {
			continue
		}
		if err := m.store.Write(stemKey(jobID, name), audio); err != nil {
			return Result{}, fmt.Errorf("persist stem %q: %w", name, err)
		}
		stems = append(stems, name)
	}
	if len(wanted) > 0 && len(stems) != len(wanted) {
		got := make(map[string]bool, len(stems))
		for _, s := range stems {
			got[strings.ToLower(s)] = true
		}
		for name := range wanted {
			if !got[name] {
				return Result{}, fmt.Errorf("%s does not produce a %q stem", separator.ID(), name)
			}
		}
	}
	sort.Strings(stems)
	return Result{ProviderID: separated.ProviderID, Stems: stems}, nil
}

func stemKey(jobID, stem string) string {
	return jobID + "_" + stem
}

func clone(job *Job) *Job {
	copy := *job
	return &copy
}

func newID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create separation job id: %w", err)
	}
	return "separation_" + hex.EncodeToString(value[:]), nil
}
