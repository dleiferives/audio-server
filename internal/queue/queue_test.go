package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dleiferives/audio-server/internal/provider"
)

type fakeProvider struct {
	id        string
	mu        sync.Mutex
	calls     []string
	started   time.Time
	active    int
	maxActive int
	delay     time.Duration
	failWith  error
}

func (f *fakeProvider) ID() string                                               { return f.id }
func (f *fakeProvider) Health(context.Context) error                             { return nil }
func (f *fakeProvider) Voices(context.Context, string) ([]provider.Voice, error) { return nil, nil }

func (f *fakeProvider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Input)
	f.started = time.Now()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.failWith != nil {
		return provider.SpeechResult{}, f.failWith
	}
	return provider.SpeechResult{Audio: []byte(req.Input), ProviderID: f.id}, nil
}

func (f *fakeProvider) startTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeProvider) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeProvider) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

type fakeLifecycle struct {
	*fakeProvider
	mu        sync.Mutex
	warmCalls int
	idleCalls int
	warmErr   error
}

func (f *fakeLifecycle) Warm(context.Context) error {
	f.mu.Lock()
	f.warmCalls++
	f.mu.Unlock()
	return f.warmErr
}

func (f *fakeLifecycle) Idle(context.Context) error {
	f.mu.Lock()
	f.idleCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeLifecycle) counts() (warm, idle int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.warmCalls, f.idleCalls
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestSubmitAndWaitSucceeds(t *testing.T) {
	p := &fakeProvider{id: "fake"}
	m := NewManager(Config{Providers: map[string]provider.Provider{"fake": p}})

	job, err := m.Submit("fake", provider.SpeechRequest{Input: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Wait(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || string(got.Result.Audio) != "hello" {
		t.Fatalf("unexpected job: %+v", got)
	}
}

func TestRunGateWrapsSynthesis(t *testing.T) {
	p := &fakeProvider{id: "gpu"}
	acquired := false
	released := false
	m := NewManager(Config{
		Providers: map[string]provider.Provider{"gpu": p},
		RunGate: func(id string) (func(), error) {
			if id != "gpu" {
				t.Fatalf("gate provider = %q", id)
			}
			acquired = true
			return func() { released = true }, nil
		},
	})
	job, _ := m.Submit("gpu", provider.SpeechRequest{Input: "hello"})
	result, err := m.Wait(context.Background(), job.ID)
	if err != nil || result.Status != StatusSucceeded {
		t.Fatalf("job failed: result=%+v err=%v", result, err)
	}
	if !acquired || !released {
		t.Fatalf("gate lifecycle acquired=%v released=%v", acquired, released)
	}
}

func TestRunGateFailureSkipsSynthesis(t *testing.T) {
	p := &fakeProvider{id: "gpu"}
	m := NewManager(Config{
		Providers: map[string]provider.Provider{"gpu": p},
		RunGate: func(string) (func(), error) {
			return nil, errors.New("no VRAM")
		},
	})
	job, _ := m.Submit("gpu", provider.SpeechRequest{Input: "hello"})
	result, err := m.Wait(context.Background(), job.ID)
	if err != nil || result.Status != StatusFailed || !strings.Contains(result.Err.Error(), "no VRAM") {
		t.Fatalf("unexpected result=%+v err=%v", result, err)
	}
	if p.callCount() != 0 {
		t.Fatal("provider ran after gate acquisition failed")
	}
}

func TestSubmitUnknownProvider(t *testing.T) {
	m := NewManager(Config{Providers: map[string]provider.Provider{}})
	_, err := m.Submit("nope", provider.SpeechRequest{})
	if !errors.Is(err, provider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestSynthesizeFailureMarksJobFailed(t *testing.T) {
	p := &fakeProvider{id: "fake", failWith: provider.ErrUnavailable}
	m := NewManager(Config{Providers: map[string]provider.Provider{"fake": p}})
	job, _ := m.Submit("fake", provider.SpeechRequest{Input: "x"})
	got, err := m.Wait(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || !errors.Is(got.Err, provider.ErrUnavailable) {
		t.Fatalf("unexpected job: %+v", got)
	}
}

func TestSingleWorkerProcessesFIFO(t *testing.T) {
	p := &fakeProvider{id: "fake", delay: 20 * time.Millisecond}
	m := NewManager(Config{Providers: map[string]provider.Provider{"fake": p}, Workers: map[string]int{"fake": 1}})

	var jobs []*Job
	for _, text := range []string{"a", "b", "c"} {
		job, err := m.Submit("fake", provider.SpeechRequest{Input: text})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	// give the first job a moment to start so the rest are observably queued
	waitFor(t, time.Second, func() bool { return p.callCount() >= 1 })
	if pos := m.Position(jobs[2].ID); pos != 2 {
		t.Fatalf("expected job c queued at position 2, got %d", pos)
	}

	for _, job := range jobs {
		if _, err := m.Wait(context.Background(), job.ID); err != nil {
			t.Fatal(err)
		}
	}
	if order := p.callOrder(); len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("unexpected call order: %v", order)
	}
}

func TestWaitReturnsOnContextCancelButJobKeepsRunning(t *testing.T) {
	p := &fakeProvider{id: "fake", delay: 50 * time.Millisecond}
	m := NewManager(Config{Providers: map[string]provider.Provider{"fake": p}})
	job, _ := m.Submit("fake", provider.SpeechRequest{Input: "x"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := m.Wait(ctx, job.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	waitFor(t, time.Second, func() bool {
		got, _ := m.Get(job.ID)
		return got.Status == StatusSucceeded
	})
}

func TestLifecycleWarmCalledOnceThenIdleAfterUnloadDelay(t *testing.T) {
	base := &fakeProvider{id: "gpu"}
	lc := &fakeLifecycle{fakeProvider: base}
	m := NewManager(Config{
		Providers:  map[string]provider.Provider{"gpu": lc},
		IdleUnload: map[string]time.Duration{"gpu": 20 * time.Millisecond},
	})

	job1, _ := m.Submit("gpu", provider.SpeechRequest{Input: "a"})
	job2, _ := m.Submit("gpu", provider.SpeechRequest{Input: "b"})
	if _, err := m.Wait(context.Background(), job1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), job2.ID); err != nil {
		t.Fatal(err)
	}

	warm, idle := lc.counts()
	if warm != 1 || idle != 0 {
		t.Fatalf("expected 1 warm, 0 idle right after two back-to-back jobs, got warm=%d idle=%d", warm, idle)
	}

	waitFor(t, time.Second, func() bool {
		_, idleCalls := lc.counts()
		return idleCalls == 1
	})

	job3, _ := m.Submit("gpu", provider.SpeechRequest{Input: "c"})
	if _, err := m.Wait(context.Background(), job3.ID); err != nil {
		t.Fatal(err)
	}
	warm, idle = lc.counts()
	if warm != 2 || idle != 1 {
		t.Fatalf("expected re-warm after idle-unload, got warm=%d idle=%d", warm, idle)
	}
}

func TestLifecycleWarmFailureFailsJobWithoutSynthesizing(t *testing.T) {
	base := &fakeProvider{id: "gpu"}
	lc := &fakeLifecycle{fakeProvider: base, warmErr: errors.New("no vram")}
	m := NewManager(Config{Providers: map[string]provider.Provider{"gpu": lc}})

	job, _ := m.Submit("gpu", provider.SpeechRequest{Input: "a"})
	got, err := m.Wait(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("expected failed job, got %+v", got)
	}
	if base.callCount() != 0 {
		t.Fatalf("expected Synthesize not to be called when Warm fails")
	}
}

func TestExclusiveResourceWaitsForPreviousProviderAndHandoffDelay(t *testing.T) {
	firstBase := &fakeProvider{id: "first", delay: 40 * time.Millisecond}
	secondBase := &fakeProvider{id: "second"}
	first := &fakeLifecycle{fakeProvider: firstBase}
	second := &fakeLifecycle{fakeProvider: secondBase}
	m := NewManager(Config{
		Providers: map[string]provider.Provider{"first": first, "second": second},
		Workers:   map[string]int{"first": 1, "second": 1},
		ResourceGroups: map[string]string{
			"first":  "gpu",
			"second": "gpu",
		},
		ResourceSwitchDelay: map[string]time.Duration{"gpu": 30 * time.Millisecond},
	})

	job1, _ := m.Submit("first", provider.SpeechRequest{Input: "first"})
	waitFor(t, time.Second, func() bool { return firstBase.callCount() == 1 })
	job2, _ := m.Submit("second", provider.SpeechRequest{Input: "second"})
	time.Sleep(10 * time.Millisecond)
	if secondBase.callCount() != 0 {
		t.Fatal("second provider started while first provider was still active")
	}

	firstResult, err := m.Wait(context.Background(), job1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), job2.ID); err != nil {
		t.Fatal(err)
	}
	if firstResult.Status != StatusSucceeded {
		t.Fatalf("first job status = %s", firstResult.Status)
	}
	if started := secondBase.startTime(); started.Sub(firstResult.FinishedAt) < 25*time.Millisecond {
		t.Fatalf("second provider started too soon after first finished: %s", started.Sub(firstResult.FinishedAt))
	}
}

func TestExclusiveResourceAllowsSameProviderConcurrency(t *testing.T) {
	p := &fakeProvider{id: "gpu", delay: 40 * time.Millisecond}
	m := NewManager(Config{
		Providers: map[string]provider.Provider{"gpu": p},
		Workers:   map[string]int{"gpu": 2},
		ResourceGroups: map[string]string{
			"gpu": "shared-gpu",
		},
	})
	job1, _ := m.Submit("gpu", provider.SpeechRequest{Input: "a"})
	job2, _ := m.Submit("gpu", provider.SpeechRequest{Input: "b"})
	if _, err := m.Wait(context.Background(), job1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), job2.ID); err != nil {
		t.Fatal(err)
	}
	if got := p.maxConcurrent(); got != 2 {
		t.Fatalf("max concurrent syntheses = %d, want 2", got)
	}
}

func TestSweepRemovesOldFinishedJobs(t *testing.T) {
	p := &fakeProvider{id: "fake"}
	current := time.Now()
	m := NewManager(Config{
		Providers: map[string]provider.Provider{"fake": p},
		JobTTL:    time.Minute,
		Now:       func() time.Time { return current },
	})
	job, _ := m.Submit("fake", provider.SpeechRequest{Input: "x"})
	if _, err := m.Wait(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	current = current.Add(2 * time.Minute)
	m.sweep()
	if _, ok := m.Get(job.ID); ok {
		t.Fatal("expected job to be swept")
	}
}
