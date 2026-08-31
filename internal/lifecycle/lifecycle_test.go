package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegisterCommandStartsAndSwitchesExclusively(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	m := NewManager("unused")
	m.RegisterCommand("python", "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, health.URL, 0)
	m.RegisterCommand("native", "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, health.URL, 0)
	t.Cleanup(m.StopAll)

	if err := m.Start("python", true); err != nil {
		t.Fatal(err)
	}
	if m.instances["python"].cmd == nil {
		t.Fatal("expected first command to be running")
	}

	if err := m.Start("native", true); err != nil {
		t.Fatal(err)
	}
	if m.instances["python"].cmd != nil {
		t.Fatal("expected exclusive switch to stop first command")
	}
	if m.instances["native"].cmd == nil {
		t.Fatal("expected second command to be running")
	}
}

func TestBudgetKeepsModelsResidentAndEvictsLRU(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	m := NewManager("unused", Config{MaxVRAMMiB: 200})
	for _, name := range []string{"first", "second", "third"} {
		m.RegisterCommandModel(name, "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, health.URL, 0, 100)
	}
	t.Cleanup(m.StopAll)

	if err := m.Start("first", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := m.Start("second", false); err != nil {
		t.Fatal(err)
	}
	if m.instances["first"].cmd == nil || m.instances["second"].cmd == nil {
		t.Fatal("models that fit the budget should remain co-resident")
	}

	if err := m.Start("third", false); err != nil {
		t.Fatal(err)
	}
	if m.instances["first"].cmd != nil {
		t.Fatal("expected least-recently-used model to be evicted")
	}
	if m.instances["second"].cmd == nil || m.instances["third"].cmd == nil {
		t.Fatal("expected newer and requested models to remain resident")
	}
}

func TestAcquireProtectsActiveModelAndSerializesExecution(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	m := NewManager("unused", Config{MaxVRAMMiB: 100})
	for _, name := range []string{"first", "second"} {
		m.RegisterCommandModel(name, "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, health.URL, 0, 100)
	}
	t.Cleanup(m.StopAll)

	releaseFirst, err := m.Acquire("first")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start("second", false); err == nil || !strings.Contains(err.Error(), "insufficient idle VRAM") {
		t.Fatalf("expected active model to be protected, got %v", err)
	}

	acquiredSecond := make(chan func(), 1)
	go func() {
		release, acquireErr := m.Acquire("second")
		if acquireErr != nil {
			acquiredSecond <- nil
			return
		}
		acquiredSecond <- release
	}()
	select {
	case <-acquiredSecond:
		t.Fatal("second execution acquired the GPU before the first released it")
	case <-time.After(20 * time.Millisecond):
	}

	releaseFirst()
	select {
	case releaseSecond := <-acquiredSecond:
		if releaseSecond == nil {
			t.Fatal("second execution failed to acquire the GPU")
		}
		releaseSecond()
	case <-time.After(time.Second):
		t.Fatal("second execution did not acquire the GPU after release")
	}
}

func TestRegisterCommandCopiesArguments(t *testing.T) {
	m := NewManager("unused")
	args := []string{"server.py", "--device", "cuda"}
	m.RegisterCommand("faster-whisper", "python3", args, "http://127.0.0.1:8030/health", 0)
	args[0] = "changed.py"

	if got := m.instances["faster-whisper"].args[0]; got != "server.py" {
		t.Fatalf("registered arguments changed through caller slice: %q", got)
	}
}
