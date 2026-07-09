package encode

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/run"
)

const (
	defaultFFmpegPath = "ffmpeg"
	defaultMP3Bitrate = "48k"
)

type FFmpeg struct {
	Path      string
	Bitrate   string
	Run       run.Command
	RunStream run.StreamIOCommand
}

func NewFFmpeg(path, bitrate string) FFmpeg {
	if strings.TrimSpace(path) == "" {
		path = defaultFFmpegPath
	}
	if strings.TrimSpace(bitrate) == "" {
		bitrate = defaultMP3Bitrate
	}
	return FFmpeg{Path: path, Bitrate: bitrate, Run: run.Exec, RunStream: run.ExecStreamIO}
}

func (f FFmpeg) Health(context.Context) error {
	if _, err := exec.LookPath(f.path()); err != nil {
		return fmt.Errorf("%w: %s not found", provider.ErrUnavailable, f.path())
	}
	return nil
}

func (f FFmpeg) Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error) {
	switch normalize(format) {
	case "mp3":
		stdout, stderr, err := f.runner()(ctx, f.path(), f.mp3Args(), wav)
		if err != nil {
			return nil, "", fmt.Errorf("%w: ffmpeg mp3 encode failed: %s", provider.ErrUnavailable, commandDetail(err, stderr))
		}
		return stdout, "audio/mpeg", nil
	default:
		return nil, "", fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
}

// EncodeStream is like Encode but streams wav in and the encoded result out,
// without buffering either fully in memory.
func (f FFmpeg) EncodeStream(ctx context.Context, wav io.Reader, format string, w io.Writer) (string, error) {
	switch normalize(format) {
	case "mp3":
		stderr, err := f.streamRunner()(ctx, f.path(), f.mp3Args(), wav, w)
		if err != nil {
			return "", fmt.Errorf("%w: ffmpeg mp3 stream encode failed: %s", provider.ErrUnavailable, commandDetail(err, stderr))
		}
		return "audio/mpeg", nil
	default:
		return "", fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
}

func (f FFmpeg) mp3Args() []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "wav",
		"-i", "pipe:0",
		"-ac", "1",
		"-b:a", f.bitrate(),
		"-f", "mp3",
		"pipe:1",
	}
}

func (f FFmpeg) path() string {
	if strings.TrimSpace(f.Path) == "" {
		return defaultFFmpegPath
	}
	return f.Path
}

func (f FFmpeg) bitrate() string {
	if strings.TrimSpace(f.Bitrate) == "" {
		return defaultMP3Bitrate
	}
	return f.Bitrate
}

func (f FFmpeg) runner() run.Command {
	if f.Run == nil {
		return run.Exec
	}
	return f.Run
}

func (f FFmpeg) streamRunner() run.StreamIOCommand {
	if f.RunStream == nil {
		return run.ExecStreamIO
	}
	return f.RunStream
}

func normalize(format string) string {
	return strings.ToLower(strings.TrimSpace(format))
}

func commandDetail(err error, stderr []byte) string {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = err.Error()
	}
	return msg
}
