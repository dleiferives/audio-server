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
	"time"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	ProviderID       string
	BaseURL          string
	Client           HTTPClient
	StartFunc        func() error
	LiveEnabled      bool
	LiveAutoLanguage bool
}

func (p Provider) SupportsLiveStreaming() bool { return p.LiveEnabled }

var _ sttprovider.Provider = Provider{}
var _ sttprovider.OnDemandProvider = Provider{}
var _ sttprovider.LiveStreamingProvider = Provider{}

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

type liveStream struct {
	provider Provider
	closed   bool
}

func (p Provider) OpenLiveStream(ctx context.Context, req sttprovider.LiveStreamRequest) (sttprovider.LiveStream, error) {
	if !p.LiveEnabled {
		return nil, fmt.Errorf("%w: model %q does not support live streaming", sttprovider.ErrInvalidRequest, p.ProviderID)
	}
	if req.SampleRate != 16000 || req.Channels != 1 {
		return nil, fmt.Errorf("%w: live transcribe.cpp audio must be mono 16 kHz PCM16", sttprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return nil, fmt.Errorf("%w: lifecycle start failed: %v", sttprovider.ErrUnavailable, err)
		}
	}
	headers := make(http.Header)
	if language := strings.TrimSpace(req.Language); language != "" && !p.LiveAutoLanguage {
		headers.Set("X-Transcribe-Language", language)
	}
	if _, err := p.liveRequest(ctx, "/stream/begin", nil, headers); err != nil {
		return nil, err
	}
	return &liveStream{provider: p}, nil
}

func (s *liveStream) Feed(ctx context.Context, pcm []byte) (sttprovider.LiveStreamUpdate, error) {
	if s.closed {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: live stream is closed", sttprovider.ErrInvalidRequest)
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: PCM frame must contain complete 16-bit samples", sttprovider.ErrInvalidRequest)
	}
	return s.provider.liveRequest(ctx, "/stream/feed", pcm, nil)
}

func (s *liveStream) Finalize(ctx context.Context) (sttprovider.LiveStreamUpdate, error) {
	if s.closed {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: live stream is closed", sttprovider.ErrInvalidRequest)
	}
	update, err := s.provider.liveRequest(ctx, "/stream/finalize", nil, nil)
	if err == nil {
		s.closed = true
	}
	return update, err
}

func (s *liveStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := s.provider.liveRequest(ctx, "/stream/reset", nil, nil)
	return err
}

func (p Provider) liveRequest(ctx context.Context, path string, pcm []byte, headers http.Header) (sttprovider.LiveStreamUpdate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, bytes.NewReader(pcm))
	if err != nil {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if pcm != nil {
		req.Header.Set("Content-Type", "audio/pcm;rate=16000;channels=1")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: %s sidecar unreachable: %v", sttprovider.ErrUnavailable, p.ProviderID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	var parsed struct {
		Text          string `json:"text"`
		CommittedText string `json:"committed_text"`
		TentativeText string `json:"tentative_text"`
		InputMS       int64  `json:"input_ms"`
		BufferedMS    int64  `json:"buffered_ms"`
		Revision      int    `json:"revision"`
		Changed       bool   `json:"changed"`
		Final         bool   `json:"final"`
		Error         struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: invalid live sidecar response: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: %s", sttprovider.ErrUnavailable, parsed.Error.Message)
	}
	return sttprovider.LiveStreamUpdate{
		Text: parsed.Text, CommittedText: parsed.CommittedText, TentativeText: parsed.TentativeText,
		InputMS: parsed.InputMS, BufferedMS: parsed.BufferedMS, Revision: parsed.Revision,
		Changed: parsed.Changed, Final: parsed.Final,
	}, nil
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
