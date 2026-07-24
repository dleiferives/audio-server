// Package supertonic implements provider.Provider by calling the
// audio.cpp native Supertonic engine via audiocpp_server.
package supertonic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
	"golang.org/x/text/unicode/norm"
)

const (
	id           = "supertonic"
	defaultVoice = "M1"
	defaultLang  = "en"
	defaultSteps = 8
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
	_ provider.Provider = Provider{}
	_ provider.Streamer = Provider{}
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

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: supertonic sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: supertonic sidecar health status %d", provider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

var presetVoices = []provider.Voice{
	{Provider: id, Voice: "M1", Language: "en", Name: "Male 1"},
	{Provider: id, Voice: "M2", Language: "en", Name: "Male 2"},
	{Provider: id, Voice: "M3", Language: "en", Name: "Male 3"},
	{Provider: id, Voice: "M4", Language: "en", Name: "Male 4"},
	{Provider: id, Voice: "M5", Language: "en", Name: "Male 5"},
	{Provider: id, Voice: "F1", Language: "en", Name: "Female 1"},
	{Provider: id, Voice: "F2", Language: "en", Name: "Female 2"},
	{Provider: id, Voice: "F3", Language: "en", Name: "Female 3"},
	{Provider: id, Voice: "F4", Language: "en", Name: "Female 4"},
	{Provider: id, Voice: "F5", Language: "en", Name: "Female 5"},
}

func (p Provider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	voices := make([]provider.Voice, 0, len(presetVoices))
	language = normalizeLanguage(language)
	for _, v := range presetVoices {
		if language != "" && language != "en" {
			v.Language = language
		}
		voices = append(voices, v)
	}
	return voices, nil
}

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
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func normalizeLanguage(language string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(language, "_", "-")))
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
	voice := req.Voice
	if strings.TrimSpace(voice) == "" || voice == "auto" {
		voice = defaultVoice
	}

	// NFC-normalize text: combining accents → precomposed chars
	// (fixes Greek codepoint errors with the unicode indexer)
	input := norm.NFC.String(req.Input)

	options := map[string]any{
		"num_inference_steps": defaultSteps,
	}
	if req.Speed > 0 && req.Speed != defaultSpeed {
		options["speed"] = req.Speed
	}

	sr := speechRequest{
		Model:          id,
		Input:          input,
		Voice:          voice,
		Language:       language,
		Speed:          speed,
		ResponseFormat: "wav",
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

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: supertonic sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.SpeechResult{}, fmt.Errorf("%w: supertonic synthesis failed: %s", provider.ErrUnavailable, synthesizeErrorDetail(resp.StatusCode, respBody))
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
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}

// SynthesizeStream implements provider.Streamer using audiocpp_server's
// SSE streaming endpoint for Supertonic.
func (p Provider) SynthesizeStream(ctx context.Context, req provider.SpeechRequest, onHeader func(provider.StreamMeta), w io.Writer) error {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return fmt.Errorf("%w: lifecycle start failed: %v", provider.ErrUnavailable, err)
		}
	}

	language := normalizeLanguage(req.Language)
	if language == "" {
		language = defaultLang
	}
	voice := req.Voice
	if strings.TrimSpace(voice) == "" || voice == "auto" {
		voice = defaultVoice
	}
	speed := req.Speed
	if speed <= 0 {
		speed = defaultSpeed
	}
	input := norm.NFC.String(req.Input)

	options := map[string]any{
		"num_inference_steps": defaultSteps,
		"text_chunk_size":     200,
		"text_chunk_mode":     "tag_aware",
	}
	if req.Speed > 0 && req.Speed != defaultSpeed {
		options["speed"] = req.Speed
	}

	sr := speechRequest{
		Model:          id,
		Input:          input,
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
		return fmt.Errorf("%w: supertonic stream unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: supertonic stream failed: %s", provider.ErrUnavailable, synthesizeErrorDetail(resp.StatusCode, respBody))
	}

	headerSent := false
	return readSSEEvents(resp.Body, func(event SSEEvent) error {
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
				return fmt.Errorf("%w: supertonic stream write: %v", provider.ErrUnavailable, err)
			}
		case "done", "speech.audio.done":
			return io.EOF
		}
		return nil
	})
}
