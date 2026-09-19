// Package cosyvoice implements provider.Provider by calling the CosyVoice2
// TTS HTTP sidecar (tts/cosyvoice/server.py). CosyVoice2's zero-shot cloning
// mode conditions on an audio prompt plus its matching transcript.
package cosyvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
)

const (
	id           = "cosyvoice"
	defaultVoice = "auto"
	defaultLang  = "auto"
	defaultSpeed = 1.0
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Encoder interface {
	Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error)
}

type ReferenceNormalizer interface {
	NormalizeReferenceAudio(ctx context.Context, audio []byte) ([]byte, error)
}

type Provider struct {
	BaseURL             string
	Client              HTTPClient
	Encoder             Encoder
	ReferenceNormalizer ReferenceNormalizer
	StartFunc           func() error
}

var (
	_ provider.Provider               = Provider{}
	_ provider.Lifecycle              = Provider{}
	_ provider.ReferenceAudioProvider = Provider{}
)

func New(baseURL string, client HTTPClient, enc Encoder) Provider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if client == nil {
		client = http.DefaultClient
	}
	normalizer, _ := enc.(ReferenceNormalizer)
	return Provider{BaseURL: baseURL, Client: client, Encoder: enc, ReferenceNormalizer: normalizer}
}

func (p Provider) ID() string { return id }

func (p Provider) SupportsAutoLanguage() bool { return true }

func (p Provider) ReferenceAudioCapabilities() provider.ReferenceAudioCapabilities {
	return provider.ReferenceAudioCapabilities{CombinedSpeakerEmotion: true}
}

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: cosyvoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: cosyvoice sidecar health status %d", provider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

func (p Provider) Warm(ctx context.Context) error {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return fmt.Errorf("%w: lifecycle start failed: %v", provider.ErrUnavailable, err)
		}
	}
	return p.postControl(ctx, "/load")
}

func (p Provider) Idle(ctx context.Context) error {
	return p.postControl(ctx, "/unload")
}

func (p Provider) postControl(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: cosyvoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: cosyvoice %s failed: %s", provider.ErrUnavailable, path, errorDetail(resp.StatusCode, body))
	}
	return nil
}

func (p Provider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	language = normalizeLanguage(language)
	if language == "" {
		language = defaultLang
	}
	return []provider.Voice{{Provider: id, Voice: defaultVoice, Language: language, Name: "CosyVoice2 voice clone"}}, nil
}

func (p Provider) Languages(context.Context) ([]string, error) {
	// CosyVoice2 is multilingual (zh/en/ja/ko + Chinese dialects) but
	// doesn't take an explicit language selector for zero-shot cloning —
	// it infers from the text and prompt.
	return []string{defaultLang, "zh", "en", "ja", "ko"}, nil
}

// Options holds CosyVoice-specific knobs, sent by the caller in
// SpeechRequest.ProviderOptions.
type Options struct {
	Seed int `json:"seed,omitempty"`
}

func parseOptions(raw json.RawMessage) (Options, error) {
	var opts Options
	if len(raw) == 0 {
		return opts, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		return Options{}, fmt.Errorf("%w: invalid provider_options: %v", provider.ErrInvalidRequest, err)
	}
	return opts, nil
}

type synthesizeRequest struct {
	Text               string  `json:"text"`
	Language           string  `json:"language,omitempty"`
	Speed              float64 `json:"speed,omitempty"`
	ReferenceAudioPath string  `json:"reference_audio_path"`
	ReferenceText      string  `json:"reference_text"`
	Seed               int     `json:"seed,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func errorDetail(status int, body []byte) string {
	var parsed errorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}

func (p Provider) writeReference(ctx context.Context, audio []byte) (string, func(), error) {
	if p.ReferenceNormalizer == nil {
		return "", nil, fmt.Errorf("%w: speaker reference normalization is not configured", provider.ErrUnavailable)
	}
	wav, err := p.ReferenceNormalizer.NormalizeReferenceAudio(ctx, audio)
	if err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp("", "audio-server-cosyvoice-reference-*.wav")
	if err != nil {
		return "", nil, fmt.Errorf("%w: create speaker reference: %v", provider.ErrUnavailable, err)
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := file.Write(wav); err != nil {
		file.Close()
		cleanup()
		return "", nil, fmt.Errorf("%w: write speaker reference: %v", provider.ErrUnavailable, err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: close speaker reference: %v", provider.ErrUnavailable, err)
	}
	return name, cleanup, nil
}

func (p Provider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return provider.SpeechResult{}, fmt.Errorf("%w: lifecycle start failed: %v", provider.ErrUnavailable, err)
		}
	}
	if len(req.SpeakerReference) == 0 {
		return provider.SpeechResult{}, fmt.Errorf("%w: cosyvoice requires a speaker_reference clip to clone", provider.ErrInvalidRequest)
	}
	referenceText := strings.TrimSpace(req.SpeakerReferenceText)
	if referenceText == "" {
		return provider.SpeechResult{}, fmt.Errorf("%w: speaker_reference_text is required with speaker_reference for cosyvoice's zero-shot prompt", provider.ErrInvalidRequest)
	}

	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	if format != "wav" && p.Encoder == nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %s", provider.ErrUnsupportedFormat, format)
	}

	language := normalizeLanguage(req.Language)
	if language == "" {
		language = defaultLang
	}
	speed := req.Speed
	if speed <= 0 {
		speed = defaultSpeed
	}
	opts, err := parseOptions(req.ProviderOptions)
	if err != nil {
		return provider.SpeechResult{}, err
	}

	refPath, cleanup, err := p.writeReference(ctx, req.SpeakerReference)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	defer cleanup()

	body, err := json.Marshal(synthesizeRequest{
		Text: req.Input, Language: language, Speed: speed,
		ReferenceAudioPath: refPath, ReferenceText: referenceText, Seed: opts.Seed,
	})
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/synthesize", bytes.NewReader(body))
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: cosyvoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.SpeechResult{}, fmt.Errorf("%w: cosyvoice synthesis failed: %s", provider.ErrUnavailable, errorDetail(resp.StatusCode, respBody))
	}

	if format == "wav" {
		return provider.SpeechResult{
			Audio: respBody, ContentType: "audio/wav", ProviderID: id, Model: id, Voice: defaultVoice, Format: "wav",
		}, nil
	}
	audio, contentType, err := p.Encoder.Encode(ctx, respBody, format)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	return provider.SpeechResult{
		Audio: audio, ContentType: contentType, ProviderID: id, Model: id, Voice: defaultVoice, Format: format,
	}, nil
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	return strings.ReplaceAll(language, "_", "-")
}
