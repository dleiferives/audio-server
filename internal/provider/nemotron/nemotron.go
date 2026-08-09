// Package nemotron implements sttprovider.Provider by calling audiocpp_server's
// Nemotron 3.5 ASR endpoints.
package nemotron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

const id = "nemotron"

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Provider struct {
	BaseURL   string
	Client    HTTPClient
	StartFunc func() error
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
		return fmt.Errorf("%w: nemotron sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: nemotron sidecar health status %d", sttprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

type transcribeResponse struct {
	Text string `json:"text"`
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p Provider) Transcribe(ctx context.Context, req sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: lifecycle start failed: %v", sttprovider.ErrUnavailable, err)
		}
	}

	if len(req.Audio) == 0 {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: audio is required", sttprovider.ErrInvalidRequest)
	}

	// Write audio to temp WAV file (audiocpp_server expects a file path)
	tmpFile, err := os.CreateTemp("", "nemotron-*.wav")
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(req.Audio); err != nil {
		tmpFile.Close()
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	tmpFile.Close()

	// Build multipart form data (audiocpp_server expects multipart, not JSON)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("model", "nemotron")
	if req.Language != "" {
		w.WriteField("language", req.Language)
	}
	part, err := w.CreateFormFile("file", req.Filename)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if _, err := part.Write(req.Audio); err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	w.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/audio/transcriptions", &buf)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: nemotron sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: nemotron transcription failed: %s", sttprovider.ErrUnavailable, errorDetail(resp.StatusCode, respBody))
	}

	var parsed transcribeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: invalid transcription response: %v", sttprovider.ErrUnavailable, err)
	}
	return sttprovider.TranscriptionResult{
		Text:       parsed.Text,
		ProviderID: id,
	}, nil
}

func errorDetail(status int, body []byte) string {
	var parsed errorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}
