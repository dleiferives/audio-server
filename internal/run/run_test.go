package run

import (
	"bytes"
	"context"
	"testing"
)

func TestExecStreamWritesStdoutProgressively(t *testing.T) {
	var out bytes.Buffer
	stderr, err := ExecStream(context.Background(), "sh", []string{"-c", "printf hello"}, nil, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr=%s)", err, stderr)
	}
	if out.String() != "hello" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello")
	}
}

func TestExecStreamCapturesStderrOnFailure(t *testing.T) {
	var out bytes.Buffer
	stderr, err := ExecStream(context.Background(), "sh", []string{"-c", "echo boom >&2; exit 1"}, nil, &out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if string(stderr) != "boom\n" {
		t.Fatalf("stderr = %q, want %q", stderr, "boom\n")
	}
}

func TestExecStreamPassesStdin(t *testing.T) {
	var out bytes.Buffer
	_, err := ExecStream(context.Background(), "cat", nil, []byte("piped in"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "piped in" {
		t.Fatalf("stdout = %q, want %q", out.String(), "piped in")
	}
}

func TestExecStreamMissingBinary(t *testing.T) {
	var out bytes.Buffer
	_, err := ExecStream(context.Background(), "definitely-not-a-real-binary", nil, nil, &out)
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}
