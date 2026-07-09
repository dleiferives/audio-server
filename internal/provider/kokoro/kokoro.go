// Package kokoro implements provider.Provider by calling the Kokoro TTS
// HTTP sidecar (tts/kokoro/server.py).
package kokoro

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
	id           = "kokoro"
	defaultVoice = "af_heart"
	defaultLang  = "en-us"
	defaultSpeed = 1.0
)

// HTTPClient is satisfied by *http.Client; it allows tests to substitute a
// fake transport.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Encoder converts the sidecar's WAV output to another format. Kokoro never
// streams, so it's always safe to buffer the WAV result and encode it after
// the fact.
type Encoder interface {
	Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error)
}

type Provider struct {
	BaseURL string
	Client  HTTPClient
	Encoder Encoder
}

var (
	_ provider.Provider  = Provider{}
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

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: kokoro sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: kokoro sidecar health status %d", provider.ErrUnavailable, resp.StatusCode)
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
		return nil, fmt.Errorf("%w: kokoro sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: kokoro voices status %d: %s", provider.ErrUnavailable, resp.StatusCode, string(body))
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

// Warm loads the model into the sidecar, blocking until it's ready.
// Implements provider.Lifecycle.
func (p Provider) Warm(ctx context.Context) error {
	return p.postControl(ctx, "/load")
}

// Idle releases the sidecar's GPU resources. Implements provider.Lifecycle.
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
		return fmt.Errorf("%w: kokoro sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: kokoro %s failed: %s", provider.ErrUnavailable, path, synthesizeErrorDetail(resp.StatusCode, body))
	}
	return nil
}

type synthesizeRequest struct {
	Text     string  `json:"text"`
	Language string  `json:"language,omitempty"`
	Voice    string  `json:"voice,omitempty"`
	Speed    float64 `json:"speed,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (p Provider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
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
	voice := strings.TrimSpace(req.Voice)
	if voice == "" || voice == "auto" {
		voice = defaultVoice
	}
	speed := req.Speed
	if speed <= 0 {
		speed = defaultSpeed
	}

	body, err := json.Marshal(synthesizeRequest{
		Text:     req.Input,
		Language: language,
		Voice:    voice,
		Speed:    speed,
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
		return provider.SpeechResult{}, fmt.Errorf("%w: kokoro sidecar unreachable: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.SpeechResult{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.SpeechResult{}, fmt.Errorf("%w: kokoro synthesis failed: %s", provider.ErrUnavailable, synthesizeErrorDetail(resp.StatusCode, respBody))
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
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	return strings.ReplaceAll(language, "_", "-")
}
