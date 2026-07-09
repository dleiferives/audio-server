package encode

import (
	"context"
	"errors"
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
