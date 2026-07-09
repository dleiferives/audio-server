package encode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/provider"
)

func TestFFmpegEncodeMP3BuildsCommand(t *testing.T) {
	var gotName string
	var gotArgs []string
	var gotStdin string
	f := FFmpeg{
		Path:    "ffmpeg-test",
		Bitrate: "64k",
		Run: func(_ context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
			gotName = name
			gotArgs = append([]string(nil), args...)
			gotStdin = string(stdin)
			return []byte("mp3"), nil, nil
		},
	}

	audio, contentType, err := f.Encode(context.Background(), []byte("wav"), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "mp3" || contentType != "audio/mpeg" {
		t.Fatalf("unexpected encode result: %q %q", audio, contentType)
	}
	if gotName != "ffmpeg-test" || gotStdin != "wav" {
		t.Fatalf("unexpected command: name=%q stdin=%q", gotName, gotStdin)
	}
	want := []string{"-hide_banner", "-loglevel", "error", "-f", "wav", "-i", "pipe:0", "-ac", "1", "-b:a", "64k", "-f", "mp3", "pipe:1"}
	if !equal(gotArgs, want) {
		t.Fatalf("args mismatch:\n got %v\nwant %v", gotArgs, want)
	}
}

func TestFFmpegEncodeRejectsUnsupportedFormat(t *testing.T) {
	_, _, err := NewFFmpeg("", "").Encode(context.Background(), []byte("wav"), "opus")
	if !errors.Is(err, provider.ErrUnsupportedFormat) {
		t.Fatalf("expected unsupported format, got %v", err)
	}
}

func TestFFmpegEncodeFailureIsUnavailable(t *testing.T) {
	f := FFmpeg{
		Path: "ffmpeg-test",
		Run: func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
			return nil, []byte("broken"), errors.New("exit 1")
		},
	}
	_, _, err := f.Encode(context.Background(), []byte("wav"), "mp3")
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func TestFFmpegEncodeStreamBuildsSameArgsAndPipes(t *testing.T) {
	var gotArgs []string
	var gotStdin string
	f := FFmpeg{
		Path:    "ffmpeg-test",
		Bitrate: "64k",
		RunStream: func(_ context.Context, name string, args []string, stdin io.Reader, w io.Writer) ([]byte, error) {
			gotArgs = append([]string(nil), args...)
			b, _ := io.ReadAll(stdin)
			gotStdin = string(b)
			_, _ = w.Write([]byte("mp3-stream"))
			return nil, nil
		},
	}

	var out bytes.Buffer
	contentType, err := f.EncodeStream(context.Background(), strings.NewReader("wav-bytes"), "mp3", &out)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "audio/mpeg" || out.String() != "mp3-stream" {
		t.Fatalf("unexpected result: contentType=%q out=%q", contentType, out.String())
	}
	if gotStdin != "wav-bytes" {
		t.Fatalf("stdin = %q, want %q", gotStdin, "wav-bytes")
	}
	want := []string{"-hide_banner", "-loglevel", "error", "-f", "wav", "-i", "pipe:0", "-ac", "1", "-b:a", "64k", "-f", "mp3", "pipe:1"}
	if !equal(gotArgs, want) {
		t.Fatalf("args mismatch:\n got %v\nwant %v", gotArgs, want)
	}
}

func TestFFmpegEncodeStreamRejectsUnsupportedFormat(t *testing.T) {
	_, err := NewFFmpeg("", "").EncodeStream(context.Background(), strings.NewReader("wav"), "opus", &bytes.Buffer{})
	if !errors.Is(err, provider.ErrUnsupportedFormat) {
		t.Fatalf("expected unsupported format, got %v", err)
	}
}

func TestFFmpegEncodeStreamFailureIsUnavailable(t *testing.T) {
	f := FFmpeg{
		Path: "ffmpeg-test",
		RunStream: func(context.Context, string, []string, io.Reader, io.Writer) ([]byte, error) {
			return []byte("broken"), errors.New("exit 1")
		},
	}
	_, err := f.EncodeStream(context.Background(), strings.NewReader("wav"), "mp3", &bytes.Buffer{})
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
