// Package fasterwhisper implements sttprovider.Provider by calling the
// faster-whisper HTTP sidecar (stt/fasterwhisper/server.py).
package fasterwhisper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

const id = "faster-whisper"

// HTTPClient is satisfied by *http.Client; it allows tests to substitute a
// fake transport.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	BaseURL string
	Client  HTTPClient
}

var _ sttprovider.Provider = Provider{}

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
		return fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: faster-whisper sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: faster-whisper sidecar health status %d", sttprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

type transcribeResponse struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (p Provider) Transcribe(ctx context.Context, req sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	if len(req.Audio) == 0 {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: audio is required", sttprovider.ErrInvalidRequest)
	}

	endpoint := p.BaseURL + "/transcribe"
	if language := strings.TrimSpace(req.Language); language != "" {
		endpoint += "?" + url.Values{"language": {language}}.Encode()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Audio))
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: faster-whisper sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: faster-whisper transcription failed: %s", sttprovider.ErrUnavailable, errorDetail(resp.StatusCode, body))
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

func errorDetail(status int, body []byte) string {
	var parsed errorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}
