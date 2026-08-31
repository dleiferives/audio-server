// Package omnivoice implements provider.Provider by calling the
// audio.cpp native OmniVoice engine via audiocpp_server.
package omnivoice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
)

const (
	id           = "omnivoice"
	defaultVoice = "auto"
	defaultLang  = "auto"
	defaultSteps = 32
	defaultSpeed = 1.0
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Encoder interface {
	Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error)
}

type Provider struct {
	BaseURL   string
	Client    HTTPClient
	Encoder   Encoder
	StartFunc func() error
}

var (
	_ provider.Provider  = Provider{}
	_ provider.Streamer  = Provider{}
	_ provider.Lifecycle = Provider{}
)

func New(baseURL string, client HTTPClient, enc Encoder) Provider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if client == nil {
		client = http.DefaultClient
	}
	return Provider{BaseURL: baseURL, Client: client, Encoder: enc}
}

func (p Provider) ID() string {
	return id
}

func (p Provider) SupportsAutoLanguage() bool { return true }

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: omnivoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: omnivoice sidecar health status %d", provider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

// Warm starts the managed sidecar, when configured, before a queued job is
// marked ready to synthesize. With no process manager this simply verifies
// that the configured sidecar is reachable.
func (p Provider) Warm(ctx context.Context) error {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return fmt.Errorf("%w: lifecycle start failed: %v", provider.ErrUnavailable, err)
		}
	}
	return p.Health(ctx)
}

// Idle is intentionally a no-op for the native audio.cpp sidecar. The shared
// lifecycle manager stops the process when another exclusive GPU provider is
// selected; the sidecar itself also lazy-loads on the next synthesis.
func (p Provider) Idle(context.Context) error { return nil }

type voicesResponse struct {
	Voices []string `json:"voices"`
}

func (p Provider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	language = normalizeLanguage(language)
	voiceLanguage := language
	if voiceLanguage == "" {
		voiceLanguage = defaultLang
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/v1/audio/voices?model="+id, nil)
	if err != nil {
		return staticVoices(voiceLanguage), nil
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return staticVoices(voiceLanguage), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return staticVoices(voiceLanguage), nil
	}
	if resp.StatusCode != http.StatusOK {
		return staticVoices(voiceLanguage), nil
	}
	var parsed voicesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return staticVoices(voiceLanguage), nil
	}
	voices := make([]provider.Voice, 0, len(parsed.Voices))
	for _, v := range parsed.Voices {
		voices = append(voices, provider.Voice{
			Provider: id,
			Voice:    v,
			Language: voiceLanguage,
			Name:     v,
		})
	}
	if len(voices) == 0 {
		return staticVoices(voiceLanguage), nil
	}
	return voices, nil
}

func staticVoices(language string) []provider.Voice {
	return []provider.Voice{{Provider: id, Voice: defaultVoice, Language: language, Name: "OmniVoice automatic"}}
}

func (p Provider) Languages(context.Context) ([]string, error) {
	return []string{defaultLang}, nil
}

// Options holds OmniVoice-specific knobs, sent by the caller in
// SpeechRequest.ProviderOptions.
type Options struct {
	Steps          int     `json:"steps,omitempty"`
	Instruct       string  `json:"instruct,omitempty"`
	GuidanceScale  float64 `json:"guidance_scale,omitempty"`
	VoiceRef       string  `json:"voice_ref,omitempty"`
	ReferenceText  string  `json:"reference_text,omitempty"`
	Seed           int     `json:"seed,omitempty"`
	ChunkSeconds   float64 `json:"chunk_seconds,omitempty"`
	ChunkThreshold float64 `json:"chunk_threshold,omitempty"`
}

func parseOptions(raw json.RawMessage) (Options, error) {
	opts := Options{
		Steps:         defaultSteps,
		GuidanceScale: 2.0,
		Seed:          42,
	}
	if len(raw) == 0 {
		return opts, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		return Options{}, fmt.Errorf("%w: invalid provider_options: %v", provider.ErrInvalidRequest, err)
	}
	if opts.Steps < 1 {
		return Options{}, fmt.Errorf("%w: provider_options.steps must be at least 1", provider.ErrInvalidRequest)
	}
	return opts, nil
}

func sidecarOptions(opts Options) map[string]any {
	options := map[string]any{
		"num_inference_steps": opts.Steps,
		"guidance_scale":      opts.GuidanceScale,
		"seed":                opts.Seed,
	}
	if opts.Instruct != "" {
		options["instruct"] = opts.Instruct
	}
	if opts.VoiceRef != "" {
		options["voice_ref"] = opts.VoiceRef
	}
	if opts.ReferenceText != "" {
		options["reference_text"] = opts.ReferenceText
	}
	if opts.ChunkSeconds > 0 {
		options["audio_chunk_duration"] = opts.ChunkSeconds
	}
	if opts.ChunkThreshold > 0 {
		options["audio_chunk_threshold"] = opts.ChunkThreshold
	}
	return options
}

// speechRequest matches the OpenAI-compatible JSON body that audiocpp_server expects.
type speechRequest struct {
	Model          string         `json:"model"`
	Input          string         `json:"input"`
	Voice          string         `json:"voice,omitempty"`
	Language       string         `json:"language,omitempty"`
	Speed          float64        `json:"speed,omitempty"`
	ResponseFormat string         `json:"response_format,omitempty"`
	StreamFormat   string         `json:"stream_format,omitempty"`
	Options        map[string]any `json:"options,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (p Provider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return provider.SpeechResult{}, fmt.Errorf("%w: lifecycle start failed: %v", provider.ErrUnavailable, err)
		}
	}

	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	if format != "wav" && format != "pcm" && p.Encoder == nil {
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
	voice := req.Voice
	if strings.TrimSpace(voice) == "" || voice == "auto" {
		voice = defaultVoice
	}
	opts, err := parseOptions(req.ProviderOptions)
	if err != nil {
		return provider.SpeechResult{}, err
	}

	options := sidecarOptions(opts)

	sidecarFormat := ""
	streamFormat := ""
	if format == "pcm" {
		// Lifecycle-managed requests use the queue's buffered path. Ask
		// audio.cpp for its chunked raw-PCM response so PCM still works
		// without bypassing the shared GPU scheduler.
		sidecarFormat = "pcm"
		streamFormat = "audio"
	}
	sr := speechRequest{
		Model:          id,
		Input:          req.Input,
		Voice:          voice,
		Language:       language,
		Speed:          speed,
		ResponseFormat: sidecarFormat,
		StreamFormat:   streamFormat,
		Options:        options,
	}

	body, err := json.Marshal(sr)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if format == "pcm" {
		httpReq.Header.Set("Accept", "audio/pcm")
	}

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: omnivoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.SpeechResult{}, fmt.Errorf("%w: omnivoice synthesis failed: %s", provider.ErrUnavailable, synthesizeErrorDetail(resp.StatusCode, respBody))
	}

	if format == "wav" {
		return provider.SpeechResult{
			Audio:       respBody,
			ContentType: "audio/wav",
			ProviderID:  id,
			Model:       id,
			Voice:       voice,
			Format:      "wav",
		}, nil
	}
	if format == "pcm" {
		return provider.SpeechResult{
			Audio:       respBody,
			ContentType: "audio/pcm",
			ProviderID:  id,
			Model:       id,
			Voice:       voice,
			Format:      "pcm",
		}, nil
	}

	audio, contentType, err := p.Encoder.Encode(ctx, respBody, format)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	return provider.SpeechResult{
		Audio:       audio,
		ContentType: contentType,
		ProviderID:  id,
		Model:       id,
		Voice:       voice,
		Format:      format,
	}, nil
}

func synthesizeErrorDetail(status int, body []byte) string {
	var parsed errorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	return strings.ReplaceAll(language, "_", "-")
}

// SynthesizeStream implements provider.Streamer using audiocpp_server's
// SSE streaming endpoint. It requests chunked audio via text-chunking,
// writes each decoded PCM chunk to w as it arrives, and calls onHeader
// once before any bytes are written.
func (p Provider) SynthesizeStream(ctx context.Context, req provider.SpeechRequest, onHeader func(provider.StreamMeta), w io.Writer) error {
	language := normalizeLanguage(req.Language)
	if language == "" {
		language = defaultLang
	}
	speed := req.Speed
	if speed <= 0 {
		speed = defaultSpeed
	}
	voice := req.Voice
	if strings.TrimSpace(voice) == "" || voice == "auto" {
		voice = defaultVoice
	}
	opts, err := parseOptions(req.ProviderOptions)
	if err != nil {
		return err
	}

	options := sidecarOptions(opts)
	options["text_chunk_size"] = 200
	options["text_chunk_mode"] = "tag_aware"

	sr := speechRequest{
		Model:          id,
		Input:          req.Input,
		Voice:          voice,
		Language:       language,
		Speed:          speed,
		ResponseFormat: "pcm",
		StreamFormat:   "sse",
		Options:        options,
	}

	body, err := json.Marshal(sr)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: omnivoice stream unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: omnivoice stream failed: %s", provider.ErrUnavailable, synthesizeErrorDetail(resp.StatusCode, respBody))
	}

	headerSent := false
	err = readSSEEvents(resp.Body, func(event SSEEvent) error {
		if !headerSent {
			onHeader(provider.StreamMeta{
				ProviderID:  id,
				Model:       id,
				Voice:       voice,
				Format:      "pcm",
				ContentType: "audio/pcm",
			})
			headerSent = true
		}
		switch event.Type {
		case "speech.audio.delta":
			chunk, err := DecodeBase64Chunk(event.Base64)
			if err != nil {
				return nil
			}
			if _, err := w.Write(chunk); err != nil {
				return fmt.Errorf("%w: omnivoice stream write: %v", provider.ErrUnavailable, err)
			}
		case "done", "speech.audio.done":
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF && !headerSent {
		return fmt.Errorf("%w: omnivoice stream read: %v", provider.ErrUnavailable, err)
	}
	return nil
}
