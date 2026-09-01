// Package sttjob runs asynchronous second-pass transcription jobs.
package sttjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

var ErrNotFound = errors.New("transcription job not found")

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Job struct {
	ID          string
	ProviderID  string
	SourceModel string
	Status      Status
	Result      sttprovider.TranscriptionResult
	Err         error
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
}

type Config struct {
	Providers map[string]sttprovider.Provider
	RunGate   func(providerID string) (func(), error)
	Timeout   time.Duration
}

type Manager struct {
	mu        sync.RWMutex
	providers map[string]sttprovider.Provider
	jobs      map[string]*Job
	runGate   func(string) (func(), error)
	timeout   time.Duration
}

func New(cfg Config) *Manager {
	return &Manager{
		providers: cfg.Providers,
		jobs:      make(map[string]*Job),
		runGate:   cfg.RunGate,
		timeout:   cfg.Timeout,
	}
}

func (m *Manager) Submit(providerID, sourceModel string, req sttprovider.TranscriptionRequest) (*Job, error) {
	p, ok := m.providers[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown post-processing transcription model %q", providerID)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job := &Job{ID: id, ProviderID: providerID, SourceModel: sourceModel, Status: StatusQueued, CreatedAt: time.Now()}
	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()
	result := clone(job)
	go m.run(job, p, req)
	return result, nil
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

func (m *Manager) run(job *Job, provider sttprovider.Provider, req sttprovider.TranscriptionRequest) {
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
	var release func()
	var err error
	if m.runGate != nil {
		release, err = m.runGate(provider.ID())
	}
	if err == nil && release != nil {
		defer release()
	}
	var result sttprovider.TranscriptionResult
	if err == nil {
		result, err = provider.Transcribe(ctx, req)
	}

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

func clone(job *Job) *Job {
	copy := *job
	return &copy
}

func newID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create transcription job id: %w", err)
	}
	return "stt_" + hex.EncodeToString(value[:]), nil
}
