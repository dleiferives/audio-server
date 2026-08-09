package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestRegisterCommandCopiesArguments(t *testing.T) {
	m := NewManager("unused")
	args := []string{"server.py", "--device", "cuda"}
	m.RegisterCommand("faster-whisper", "python3", args, "http://127.0.0.1:8030/health", 0)
	args[0] = "changed.py"

	if got := m.instances["faster-whisper"].args[0]; got != "server.py" {
		t.Fatalf("registered arguments changed through caller slice: %q", got)
	}
}
