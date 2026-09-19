// Package moonshine implements sttprovider.Provider by calling the Moonshine v2
// HTTP sidecar (stt/moonshine/server.py).
//
// This is the CPU-only Moonshine path, built on the moonshine-voice pip package.
// It is deliberately distinct from the GGUF "moonshine" model that audio.cpp
// serves on the GPU: this provider costs no VRAM and so is not registered
// against the GPU residency budget.
//
// Moonshine v2's encoder streams — it caches encoder output and part of the
// decoder state and refines the transcript as audio arrives, rather than
// re-decoding a growing buffer. So this provider implements LiveStreamingProvider
// for real, and the buffered Transcribe path is the fallback rather than the
// main event.
package moonshine

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

const id = "moonshine"

// HTTPClient is satisfied by *http.Client; it allows tests to substitute a
// fake transport.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	BaseURL   string
	Client    HTTPClient
	StartFunc func() error
}

var _ sttprovider.Provider = Provider{}
var _ sttprovider.StreamingProvider = Provider{}
var _ sttprovider.OnDemandProvider = Provider{}
var _ sttprovider.AudioRequirementsProvider = Provider{}
var _ sttprovider.LiveStreamingProvider = Provider{}

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

// AudioRequirements makes the server normalize uploads with ffmpeg before
// dispatch. The sidecar decodes with the Python stdlib wave module and accepts
// only this exact format.
func (p Provider) AudioRequirements() sttprovider.AudioFormat {
	return sttprovider.AudioFormat{Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1}
}

func (p Provider) StartsOnDemand() bool {
	return p.StartFunc != nil
}

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: moonshine sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: moonshine sidecar health status %d", sttprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

type transcribeResponse struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
}

// normalizeLanguage reduces a browser-facing BCP-47 tag (en-US, el_GR) to the
// language subtag Moonshine selects a model with. The sidecar does this too;
// doing it here keeps the header clean and the behaviour testable.
func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if separator := strings.IndexAny(language, "-_"); separator >= 0 {
		language = language[:separator]
	}
	return language
}

func (p Provider) start() error {
	if p.StartFunc == nil {
		return nil
	}
	if err := p.StartFunc(); err != nil {
		return fmt.Errorf("%w: lifecycle start failed: %v", sttprovider.ErrUnavailable, err)
	}
	return nil
}

func (p Provider) Transcribe(ctx context.Context, req sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	if len(req.Audio) == 0 {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: audio is required", sttprovider.ErrInvalidRequest)
	}
	if err := p.start(); err != nil {
		return sttprovider.TranscriptionResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/transcribe", bytes.NewReader(req.Audio))
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	if language := normalizeLanguage(req.Language); language != "" {
		httpReq.Header.Set("X-Transcribe-Language", language)
	}

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: moonshine sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: moonshine transcription failed: %s", sttprovider.ErrUnavailable, errorDetail(resp.StatusCode, body))
	}

	var parsed transcribeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: invalid transcribe response: %v", sttprovider.ErrUnavailable, err)
	}
	return sttprovider.TranscriptionResult{
		Text:       parsed.Text,
		Language:   parsed.Language,
		Duration:   parsed.Duration,
		ProviderID: id,
	}, nil
}

// TranscribeStream adapts the buffered response to the server's SSE contract for
// clients that post cumulative snapshots instead of opening a live session.
func (p Provider) TranscribeStream(
	ctx context.Context,
	req sttprovider.TranscriptionRequest,
	onPartial func(sttprovider.TranscriptionResult) error,
) (sttprovider.TranscriptionResult, error) {
	result, err := p.Transcribe(ctx, req)
	if err != nil {
		return sttprovider.TranscriptionResult{}, err
	}
	if onPartial != nil {
		if err := onPartial(result); err != nil {
			return sttprovider.TranscriptionResult{}, err
		}
	}
	return result, nil
}

// ── live streaming ──

func (p Provider) SupportsLiveStreaming() bool { return true }

type liveStream struct {
	provider Provider
	closed   bool
}

func (p Provider) OpenLiveStream(ctx context.Context, req sttprovider.LiveStreamRequest) (sttprovider.LiveStream, error) {
	if req.SampleRate != 16000 || req.Channels != 1 {
		return nil, fmt.Errorf("%w: live moonshine audio must be mono 16 kHz PCM16", sttprovider.ErrInvalidRequest)
	}
	if err := p.start(); err != nil {
		return nil, err
	}
	headers := make(http.Header)
	if language := normalizeLanguage(req.Language); language != "" {
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
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: moonshine sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
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
		Lines         []struct {
			Text       string `json:"text"`
			StartMS    int64  `json:"start_ms"`
			DurationMS int64  `json:"duration_ms"`
			Complete   bool   `json:"complete"`
		} `json:"lines"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: invalid live sidecar response: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		message := parsed.Error.Message
		if strings.TrimSpace(message) == "" {
			message = fmt.Sprintf("moonshine sidecar status %d", resp.StatusCode)
		}
		return sttprovider.LiveStreamUpdate{}, fmt.Errorf("%w: %s", sttprovider.ErrUnavailable, message)
	}
	var lines []sttprovider.LiveLine
	for _, line := range parsed.Lines {
		lines = append(lines, sttprovider.LiveLine{
			Text: line.Text, StartMS: line.StartMS, DurationMS: line.DurationMS, Complete: line.Complete,
		})
	}
	return sttprovider.LiveStreamUpdate{
		Text: parsed.Text, CommittedText: parsed.CommittedText, TentativeText: parsed.TentativeText,
		InputMS: parsed.InputMS, BufferedMS: parsed.BufferedMS, Revision: parsed.Revision,
		Changed: parsed.Changed, Final: parsed.Final, Lines: lines,
	}, nil
}

func errorDetail(status int, body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Error.Message != "" {
			return parsed.Error.Message
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}
