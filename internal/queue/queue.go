// Package queue schedules synthesis jobs per provider, so that GPU-bound
// providers (limited to one worker) don't reload their model between
// back-to-back requests, while cheap subprocess-based providers can still
// run several requests concurrently.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dleiferives/audio-server/internal/provider"
)

var ErrNotFound = errors.New("job not found")

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

const (
	defaultJobTTL           = 10 * time.Minute
	defaultLifecycleTimeout = 5 * time.Minute
	sweepInterval           = time.Minute
)

type Job struct {
	ID         string
	ProviderID string
	Request    provider.SpeechRequest
	Status     Status
	Result     provider.SpeechResult
	Err        error
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	// StreamChunks counts audio chunks received during SSE streaming.
	StreamChunks int

	// StreamBus carries PCM chunks to zero or more subscribers during
	// streaming synthesis. Closed when the stream ends.
	StreamBus chan []byte

	done chan struct{}
}

type Config struct {
	Providers map[string]provider.Provider
	// Workers is the worker pool size per provider ID. Providers not listed
	// (or with a value <= 0) get exactly 1 worker.
	Workers map[string]int
	// IdleUnload is how long a provider's queue must be empty before its
	// Lifecycle.Idle is called, per provider ID. 0 or absent disables
	// idle-unload for that provider.
	IdleUnload map[string]time.Duration
	// SynthesizeTimeout bounds each provider.Synthesize call. <= 0 means no
	// timeout.
	SynthesizeTimeout time.Duration
	// JobTTL is how long finished jobs are retained before being swept from
	// memory. <= 0 uses defaultJobTTL.
	JobTTL time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

type Manager struct {
	mu         sync.Mutex
	providers  map[string]provider.Provider
	queues     map[string][]*Job
	jobs       map[string]*Job
	conds      map[string]*sync.Cond
	active     map[string]int
	warm       map[string]bool
	idleDelay  map[string]time.Duration
	idleTimers map[string]*time.Timer
	// lifecycleMu serializes Warm/Idle calls per provider so a timer-driven
	// Idle can never run concurrently with a worker-driven Warm.
	lifecycleMu map[string]*sync.Mutex

	synthesizeTimeout time.Duration
	jobTTL            time.Duration
	now               func() time.Time
}

func NewManager(cfg Config) *Manager {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	jobTTL := cfg.JobTTL
	if jobTTL <= 0 {
		jobTTL = defaultJobTTL
	}

	m := &Manager{
		providers:         cfg.Providers,
		queues:            make(map[string][]*Job, len(cfg.Providers)),
		jobs:              make(map[string]*Job),
		conds:             make(map[string]*sync.Cond, len(cfg.Providers)),
		active:            make(map[string]int, len(cfg.Providers)),
		warm:              make(map[string]bool, len(cfg.Providers)),
		idleDelay:         make(map[string]time.Duration, len(cfg.Providers)),
		idleTimers:        make(map[string]*time.Timer, len(cfg.Providers)),
		lifecycleMu:       make(map[string]*sync.Mutex, len(cfg.Providers)),
		synthesizeTimeout: cfg.SynthesizeTimeout,
		jobTTL:            jobTTL,
		now:               now,
	}

	for id := range cfg.Providers {
		m.conds[id] = sync.NewCond(&m.mu)
		m.lifecycleMu[id] = &sync.Mutex{}
		workers := cfg.Workers[id]
		if workers <= 0 {
			workers = 1
		}
		if delay := cfg.IdleUnload[id]; delay > 0 {
			m.idleDelay[id] = delay
		}
		for i := 0; i < workers; i++ {
			go m.worker(id)
		}
	}

	go m.sweepLoop()
	return m
}

// Submit enqueues a job for providerID and returns immediately.
func (m *Manager) Submit(providerID string, req provider.SpeechRequest) (*Job, error) {
	m.mu.Lock()
	if _, ok := m.providers[providerID]; !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: unknown provider %q", provider.ErrInvalidRequest, providerID)
	}
	job := &Job{
		ID:         newID(),
		ProviderID: providerID,
		Request:    req,
		Status:     StatusQueued,
		CreatedAt:  m.now(),
		done:       make(chan struct{}),
	}
	m.jobs[job.ID] = job
	m.queues[providerID] = append(m.queues[providerID], job)
	m.cancelIdleTimerLocked(providerID)
	cond := m.conds[providerID]
	m.mu.Unlock()

	cond.Broadcast()
	return job, nil
}

// Get returns a snapshot of the job's current state.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Position returns the job's 1-based position among still-queued jobs for
// its provider, or 0 if it's already running/finished, or -1 if unknown.
func (m *Manager) Position(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return -1
	}
	if j.Status != StatusQueued {
		return 0
	}
	for i, q := range m.queues[j.ProviderID] {
		if q.ID == id {
			return i + 1
		}
	}
	return 0
}

// Wait blocks until the job finishes or ctx is done. The job itself keeps
// running in the background even if ctx is cancelled first — other queued
// jobs and future polls depend on it completing.
func (m *Manager) Wait(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	done := j.done
	m.mu.Unlock()

	select {
	case <-done:
		result, _ := m.Get(id)
		return result, nil
	case <-ctx.Done():
		return Job{}, ctx.Err()
	}
}

// NewRunningJob creates a job that is already in StatusRunning state,
// bypassing the worker queue. Used for streaming synthesis where the
// caller drives execution directly.
func (m *Manager) NewRunningJob(providerID string, req provider.SpeechRequest) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := &Job{
		ID:         newID(),
		ProviderID: providerID,
		Request:    req,
		Status:     StatusRunning,
		CreatedAt:  m.now(),
		StartedAt:  m.now(),
		done:       make(chan struct{}),
		StreamBus:  make(chan []byte, 64),
	}
	m.jobs[job.ID] = job
	return job
}

// CompleteJob marks a job as succeeded and closes its stream bus.
func (m *Manager) CompleteJob(id string, result provider.SpeechResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return
	}
	j.FinishedAt = m.now()
	j.Status = StatusSucceeded
	j.Result = result
	close(j.done)
	if j.StreamBus != nil {
		close(j.StreamBus)
		j.StreamBus = nil
	}
}

// FailJob marks a job as failed and closes its stream bus.
func (m *Manager) FailJob(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return
	}
	j.FinishedAt = m.now()
	j.Status = StatusFailed
	j.Err = err
	close(j.done)
	if j.StreamBus != nil {
		close(j.StreamBus)
		j.StreamBus = nil
	}
}

// BumpChunk increments the stream chunk counter for a running job
// and pushes the PCM bytes to all stream subscribers.
func (m *Manager) BumpChunk(id string, chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return
	}
	j.StreamChunks++
	if j.StreamBus != nil {
		select {
		case j.StreamBus <- chunk:
		default:
		}
	}
}

func (m *Manager) worker(providerID string) {
	for {
		job := m.dequeue(providerID)
		m.run(providerID, job)
	}
}

func (m *Manager) dequeue(providerID string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.queues[providerID]) == 0 {
		m.conds[providerID].Wait()
	}
	q := m.queues[providerID]
	job := q[0]
	m.queues[providerID] = q[1:]
	m.active[providerID]++
	job.Status = StatusRunning
	job.StartedAt = m.now()
	return job
}

func (m *Manager) run(providerID string, job *Job) {
	m.mu.Lock()
	p := m.providers[providerID]
	lc, hasLifecycle := p.(provider.Lifecycle)
	needWarm := hasLifecycle && !m.warm[providerID]
	m.mu.Unlock()

	if needWarm {
		lifecycleMu := m.lifecycleMu[providerID]
		lifecycleMu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), defaultLifecycleTimeout)
		err := lc.Warm(ctx)
		cancel()
		lifecycleMu.Unlock()
		if err != nil {
			m.finish(providerID, job, provider.SpeechResult{}, fmt.Errorf("model warm-up failed: %w", err))
			return
		}
		m.mu.Lock()
		m.warm[providerID] = true
		m.mu.Unlock()
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if m.synthesizeTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, m.synthesizeTimeout)
		defer cancel()
	}
	result, err := p.Synthesize(ctx, job.Request)
	m.finish(providerID, job, result, err)
}

func (m *Manager) finish(providerID string, job *Job, result provider.SpeechResult, err error) {
	m.mu.Lock()
	job.FinishedAt = m.now()
	if err != nil {
		job.Status = StatusFailed
		job.Err = err
	} else {
		job.Status = StatusSucceeded
		job.Result = result
	}
	close(job.done)
	m.active[providerID]--
	if m.active[providerID] == 0 && len(m.queues[providerID]) == 0 {
		m.startIdleTimerLocked(providerID)
	}
	m.mu.Unlock()
}

// startIdleTimerLocked must be called with m.mu held.
func (m *Manager) startIdleTimerLocked(providerID string) {
	delay, ok := m.idleDelay[providerID]
	if !ok || delay <= 0 || !m.warm[providerID] {
		return
	}
	if m.idleTimers[providerID] != nil {
		return
	}
	m.idleTimers[providerID] = time.AfterFunc(delay, func() { m.fireIdle(providerID) })
}

// cancelIdleTimerLocked must be called with m.mu held.
func (m *Manager) cancelIdleTimerLocked(providerID string) {
	if t := m.idleTimers[providerID]; t != nil {
		t.Stop()
		m.idleTimers[providerID] = nil
	}
}

func (m *Manager) fireIdle(providerID string) {
	m.mu.Lock()
	if m.active[providerID] != 0 || len(m.queues[providerID]) != 0 || !m.warm[providerID] {
		m.mu.Unlock()
		return
	}
	p := m.providers[providerID]
	lc, hasLifecycle := p.(provider.Lifecycle)
	m.warm[providerID] = false
	m.idleTimers[providerID] = nil
	m.mu.Unlock()

	if hasLifecycle {
		lifecycleMu := m.lifecycleMu[providerID]
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), defaultLifecycleTimeout)
		defer cancel()
		_ = lc.Idle(ctx)
	}
}

func (m *Manager) sweepLoop() {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		m.sweep()
	}
}

func (m *Manager) sweep() {
	cutoff := m.now().Add(-m.jobTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if (j.Status == StatusSucceeded || j.Status == StatusFailed) && j.FinishedAt.Before(cutoff) {
			delete(m.jobs, id)
		}
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
