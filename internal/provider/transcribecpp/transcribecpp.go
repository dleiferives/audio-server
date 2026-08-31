// Package transcribecpp connects the audio server to a persistent
// transcribe.cpp model sidecar.
package transcribecpp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	ProviderID string
	BaseURL    string
	Client     HTTPClient
	StartFunc  func() error
}

var _ sttprovider.Provider = Provider{}
var _ sttprovider.OnDemandProvider = Provider{}

func New(providerID, baseURL string, client HTTPClient) Provider {
	if client == nil {
		client = http.DefaultClient
	}
	return Provider{
		ProviderID: strings.TrimSpace(providerID),
		BaseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Client:     client,
	}
}

func (p Provider) ID() string { return p.ProviderID }

func (p Provider) StartsOnDemand() bool { return p.StartFunc != nil }

func (p Provider) AudioRequirements() sttprovider.AudioFormat {
	return sttprovider.AudioFormat{Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1}
}

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s sidecar unreachable: %v", sttprovider.ErrUnavailable, p.ProviderID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s health status %d", sttprovider.ErrUnavailable, p.ProviderID, resp.StatusCode)
	}
	return nil
}

func (p Provider) Transcribe(ctx context.Context, req sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	if len(req.Audio) == 0 {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: audio is required", sttprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: lifecycle start failed: %v", sttprovider.ErrUnavailable, err)
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/transcribe", bytes.NewReader(req.Audio))
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "audio/wav")
	if language := normalizeLanguage(req.Language); language != "" {
		httpReq.Header.Set("X-Transcribe-Language", language)
	}
	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %s sidecar unreachable: %v", sttprovider.ErrUnavailable, p.ProviderID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	var parsed struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: invalid sidecar response: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %s transcription failed: %s", sttprovider.ErrUnavailable, p.ProviderID, parsed.Error.Message)
	}
	return sttprovider.TranscriptionResult{
		Text:       parsed.Text,
		Language:   parsed.Language,
		Duration:   parsed.Duration,
		ProviderID: p.ProviderID,
	}, nil
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if separator := strings.IndexAny(language, "-_"); separator >= 0 {
		language = language[:separator]
	}
	return language
}
