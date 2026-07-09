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
	format = normalize(format)
	args, contentType, ok := f.argsFor(format)
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
	stdout, stderr, err := f.runner()(ctx, f.path(), args, wav)
	if err != nil {
		return nil, "", fmt.Errorf("%w: ffmpeg %s encode failed: %s", provider.ErrUnavailable, format, commandDetail(err, stderr))
	}
	return stdout, contentType, nil
}

// EncodeStream is like Encode but streams wav in and the encoded result out,
// without buffering either fully in memory.
func (f FFmpeg) EncodeStream(ctx context.Context, wav io.Reader, format string, w io.Writer) (string, error) {
	format = normalize(format)
	args, contentType, ok := f.argsFor(format)
	if !ok {
		return "", fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
	stderr, err := f.streamRunner()(ctx, f.path(), args, wav, w)
	if err != nil {
		return "", fmt.Errorf("%w: ffmpeg %s stream encode failed: %s", provider.ErrUnavailable, format, commandDetail(err, stderr))
	}
	return contentType, nil
}

// argsFor returns the ffmpeg args and response Content-Type for a
// normalized format, or ok=false if the format isn't supported.
func (f FFmpeg) argsFor(format string) (args []string, contentType string, ok bool) {
	base := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "wav",
		"-i", "pipe:0",
		"-ac", "1",
	}
	switch format {
	case "mp3":
		return append(base, "-b:a", f.bitrate(), "-f", "mp3", "pipe:1"), "audio/mpeg", true
	case "ogg":
		return append(base, "-c:a", "libvorbis", "-b:a", f.bitrate(), "-f", "ogg", "pipe:1"), "audio/ogg", true
	case "opus":
		// Opus is carried in an Ogg container; there's no distinct "opus"
		// muxer, only the libopus encoder writing into the same Ogg format.
		return append(base, "-c:a", "libopus", "-b:a", f.bitrate(), "-f", "ogg", "pipe:1"), "audio/ogg; codecs=opus", true
	case "flac":
		return append(base, "-f", "flac", "pipe:1"), "audio/flac", true
	default:
		return nil, "", false
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
