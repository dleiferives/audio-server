package espeak

import (
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/run"
)

const (
	defaultPath  = "espeak-ng"
	defaultVoice = "en"
	defaultWPM   = 175
	minWPM       = 80
	maxWPM       = 450
)

type Encoder interface {
	Health(ctx context.Context) error
	Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error)
	EncodeStream(ctx context.Context, wav io.Reader, format string, w io.Writer) (string, error)
}

type Provider struct {
	Path         string
	DefaultVoice string
	Run          run.Command
	RunStream    run.StreamCommand
	Encoder      Encoder
}

var _ provider.Streamer = Provider{}

func New(path, defaultVoice string, enc Encoder) Provider {
	if strings.TrimSpace(path) == "" {
		path = defaultPath
	}
	if strings.TrimSpace(defaultVoice) == "" {
		defaultVoice = defaultVoiceName()
	}
	return Provider{Path: path, DefaultVoice: defaultVoice, Run: run.Exec, RunStream: run.ExecStream, Encoder: enc}
}

func (p Provider) ID() string {
	return "espeak-ng"
}

func (p Provider) Health(ctx context.Context) error {
	if _, err := exec.LookPath(p.path()); err != nil {
		return fmt.Errorf("%w: %s not found", provider.ErrUnavailable, p.path())
	}
	if p.Encoder != nil {
		if err := p.Encoder.Health(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p Provider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	args := []string{"--voices"}
	if language = normalizeLanguage(language); language != "" {
		args = []string{"--voices=" + language}
	}
	stdout, stderr, err := p.runner()(ctx, p.path(), args, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: espeak voices failed: %s", provider.ErrUnavailable, commandDetail(err, stderr))
	}
	return parseVoices(p.ID(), string(stdout)), nil
}

func (p Provider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	format, voice, args := p.buildRequest(req)
	wav, stderr, err := p.runner()(ctx, p.path(), args, []byte(req.Input))
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: espeak synthesis failed: %s", provider.ErrUnavailable, commandDetail(err, stderr))
	}
	if format == "wav" {
		return provider.SpeechResult{
			Audio:       wav,
			ContentType: "audio/wav",
			ProviderID:  p.ID(),
			Model:       p.ID(),
			Voice:       voice,
			Format:      format,
		}, nil
	}
	if p.Encoder == nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
	audio, contentType, err := p.Encoder.Encode(ctx, wav, format)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	return provider.SpeechResult{
		Audio:       audio,
		ContentType: contentType,
		ProviderID:  p.ID(),
		Model:       p.ID(),
		Voice:       voice,
		Format:      format,
	}, nil
}

// SynthesizeStream implements provider.Streamer: it pipes espeak-ng's
// output directly to w as it's produced (via ffmpeg for mp3) instead of
// buffering the full result first.
func (p Provider) SynthesizeStream(ctx context.Context, req provider.SpeechRequest, onHeader func(provider.StreamMeta), w io.Writer) error {
	format, voice, args := p.buildRequest(req)
	contentType, ok := contentTypeForFormat(format)
	if !ok {
		return fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}
	if format != "wav" && p.Encoder == nil {
		return fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}

	if format == "wav" {
		onHeader(provider.StreamMeta{ProviderID: p.ID(), Model: p.ID(), Voice: voice, Format: format, ContentType: contentType})
		stderr, err := p.streamRunner()(ctx, p.path(), args, []byte(req.Input), w)
		if err != nil {
			return fmt.Errorf("%w: espeak synthesis failed: %s", provider.ErrUnavailable, commandDetail(err, stderr))
		}
		return nil
	}

	pr, pw := io.Pipe()
	espeakDone := make(chan struct{})
	var espeakErr error
	var espeakStderr []byte
	go func() {
		defer close(espeakDone)
		espeakStderr, espeakErr = p.streamRunner()(ctx, p.path(), args, []byte(req.Input), pw)
		pw.CloseWithError(espeakErr)
	}()

	onHeader(provider.StreamMeta{ProviderID: p.ID(), Model: p.ID(), Voice: voice, Format: format, ContentType: contentType})
	_, encErr := p.Encoder.EncodeStream(ctx, pr, format, w)
	<-espeakDone

	if espeakErr != nil {
		return fmt.Errorf("%w: espeak synthesis failed: %s", provider.ErrUnavailable, commandDetail(espeakErr, espeakStderr))
	}
	return encErr
}

// contentTypeForFormat mirrors the formats encode.FFmpeg supports, so
// headers can be sent before any bytes are written (format validity doesn't
// depend on the audio itself).
func contentTypeForFormat(format string) (string, bool) {
	switch format {
	case "wav":
		return "audio/wav", true
	case "mp3":
		return "audio/mpeg", true
	case "ogg":
		return "audio/ogg", true
	case "opus":
		return "audio/ogg; codecs=opus", true
	case "flac":
		return "audio/flac", true
	default:
		return "", false
	}
}

func (p Provider) buildRequest(req provider.SpeechRequest) (format, voice string, args []string) {
	format = strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "mp3"
	}
	voice = p.chooseVoice(req)
	args = []string{
		"--stdout",
		"--stdin",
		"-b", "1",
		"-v", voice,
		"-s", strconv.Itoa(WPM(req.Speed)),
	}
	return format, voice, args
}

func WPM(speed float64) int {
	if speed <= 0 {
		speed = 1
	}
	wpm := int(math.Round(defaultWPM * speed))
	if wpm < minWPM {
		return minWPM
	}
	if wpm > maxWPM {
		return maxWPM
	}
	return wpm
}

func (p Provider) chooseVoice(req provider.SpeechRequest) string {
	if voice := strings.TrimSpace(req.Voice); voice != "" && voice != "auto" {
		return voice
	}
	if language := normalizeLanguage(req.Language); language != "" {
		return language
	}
	return p.defaultVoice()
}

func (p Provider) path() string {
	if strings.TrimSpace(p.Path) == "" {
		return defaultPath
	}
	return p.Path
}

func (p Provider) defaultVoice() string {
	if strings.TrimSpace(p.DefaultVoice) == "" {
		return defaultVoiceName()
	}
	return p.DefaultVoice
}

func (p Provider) streamRunner() run.StreamCommand {
	if p.RunStream == nil {
		return run.ExecStream
	}
	return p.RunStream
}

func (p Provider) runner() run.Command {
	if p.Run == nil {
		return run.Exec
	}
	return p.Run
}

func defaultVoiceName() string {
	return defaultVoice
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	return strings.ReplaceAll(language, "_", "-")
}

func parseVoices(providerID, out string) []provider.Voice {
	var voices []provider.Voice
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.EqualFold(fields[0], "pty") {
			continue
		}
		gender := fields[2]
		if i := strings.LastIndex(gender, "/"); i >= 0 && i+1 < len(gender) {
			gender = gender[i+1:]
		}
		voices = append(voices, provider.Voice{
			Provider: providerID,
			Language: fields[1],
			Gender:   gender,
			Name:     fields[3],
			Voice:    fields[3],
		})
	}
	return voices
}

func commandDetail(err error, stderr []byte) string {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = err.Error()
	}
	return msg
}
