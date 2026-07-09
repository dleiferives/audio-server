// Package omnivoice implements provider.Provider by calling the OmniVoice
// HTTP sidecar (tts/omnivoice/server.py).
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
	id             = "omnivoice"
	defaultVoice   = "auto"
	defaultLang    = "el"
	defaultSteps   = 32
	defaultSpeed   = 1.0
	defaultSeed    = 42
	defaultChunkS  = 12.0
	defaultChunkTh = 18.0
)

// HTTPClient is satisfied by *http.Client; it allows tests to substitute a
// fake transport.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	BaseURL string
	Client  HTTPClient
}

func New(baseURL string, client HTTPClient) Provider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if client == nil {
		client = http.DefaultClient
	}
	return Provider{BaseURL: baseURL, Client: client}
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
		return fmt.Errorf("%w: omnivoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: omnivoice sidecar health status %d", provider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

type voicesResponse struct {
	Voices []struct {
		Provider string `json:"provider"`
		Voice    string `json:"voice"`
		Language string `json:"language"`
		Name     string `json:"name"`
		Gender   string `json:"gender"`
	} `json:"voices"`
}

func (p Provider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/voices", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: omnivoice sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: omnivoice voices status %d: %s", provider.ErrUnavailable, resp.StatusCode, string(body))
	}
	var parsed voicesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: invalid voices response: %v", provider.ErrUnavailable, err)
	}

	language = normalizeLanguage(language)
	voices := make([]provider.Voice, 0, len(parsed.Voices))
	for _, v := range parsed.Voices {
		if language != "" && normalizeLanguage(v.Language) != language {
			continue
		}
		voices = append(voices, provider.Voice{
			Provider: id,
			Voice:    v.Voice,
			Language: v.Language,
			Name:     v.Name,
			Gender:   v.Gender,
		})
	}
	return voices, nil
}

type synthesizeRequest struct {
	Text           string  `json:"text"`
	Language       string  `json:"language,omitempty"`
	Speed          float64 `json:"speed,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	Seed           int     `json:"seed,omitempty"`
	ChunkSeconds   float64 `json:"chunk_seconds,omitempty"`
	ChunkThreshold float64 `json:"chunk_threshold,omitempty"`
}

// Options holds OmniVoice-specific knobs, sent by the caller in
// SpeechRequest.ProviderOptions, e.g.:
//
//	{"steps": 16, "seed": 7, "chunk_seconds": 8, "chunk_threshold": 10}
type Options struct {
	Steps          int     `json:"steps,omitempty"`
	Seed           int     `json:"seed,omitempty"`
	ChunkSeconds   float64 `json:"chunk_seconds,omitempty"`
	ChunkThreshold float64 `json:"chunk_threshold,omitempty"`
}

func parseOptions(raw json.RawMessage) (Options, error) {
	opts := Options{
		Steps:          defaultSteps,
		Seed:           defaultSeed,
		ChunkSeconds:   defaultChunkS,
		ChunkThreshold: defaultChunkTh,
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
	if opts.ChunkSeconds <= 0 {
		return Options{}, fmt.Errorf("%w: provider_options.chunk_seconds must be greater than 0", provider.ErrInvalidRequest)
	}
	if opts.ChunkThreshold <= 0 {
		return Options{}, fmt.Errorf("%w: provider_options.chunk_threshold must be greater than 0", provider.ErrInvalidRequest)
	}
	return opts, nil
}

type errorResponse struct {
	Error string `json:"error"`
}

func (p Provider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format == "" {
		format = "wav"
	}
	if format != "wav" {
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

	body, err := json.Marshal(synthesizeRequest{
		Text:           req.Input,
		Language:       language,
		Speed:          speed,
		Steps:          opts.Steps,
		Seed:           opts.Seed,
		ChunkSeconds:   opts.ChunkSeconds,
		ChunkThreshold: opts.ChunkThreshold,
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

	voice := req.Voice
	if strings.TrimSpace(voice) == "" || voice == "auto" {
		voice = defaultVoice
	}
	return provider.SpeechResult{
		Audio:       respBody,
		ContentType: "audio/wav",
		ProviderID:  id,
		Model:       id,
		Voice:       voice,
		Format:      "wav",
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
