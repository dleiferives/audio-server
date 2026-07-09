package run

import (
	"bytes"
	"context"
	"io"
	"os/exec"
)

type Command func(ctx context.Context, name string, args []string, stdin []byte) (stdout, stderr []byte, err error)

func Exec(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// StreamCommand mirrors Command but writes stdout directly to the caller's
// writer as the process produces it, instead of buffering the full output.
type StreamCommand func(ctx context.Context, name string, args []string, stdin []byte, stdout io.Writer) (stderr []byte, err error)

func ExecStream(ctx context.Context, name string, args []string, stdin []byte, stdout io.Writer) ([]byte, error) {
	var r io.Reader
	if stdin != nil {
		r = bytes.NewReader(stdin)
	}
	return ExecStreamIO(ctx, name, args, r, stdout)
}

// StreamIOCommand is like StreamCommand but also streams stdin from an
// io.Reader instead of a fully-buffered []byte — needed when stdin is
// itself a live pipe (e.g. another process's stdout) rather than data
// already held in memory.
type StreamIOCommand func(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer) (stderr []byte, err error)

func ExecStreamIO(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.Bytes(), err
}
