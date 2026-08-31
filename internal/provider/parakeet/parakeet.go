// Package parakeet implements sttprovider.Provider by calling an
// audiocpp_server instance configured with Parakeet-TDT.
package parakeet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

const id = "parakeet"

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
		return fmt.Errorf("%w: parakeet sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: parakeet sidecar health status %d", sttprovider.ErrUnavailable, resp.StatusCode)
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
	httpReq, err := p.newTranscriptionRequest(ctx, req, false)
	if err != nil {
		return sttprovider.TranscriptionResult{}, err
	}
	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: parakeet sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: parakeet transcription failed: %s", sttprovider.ErrUnavailable, errorDetail(resp.StatusCode, respBody))
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

func (p Provider) TranscribeStream(
	ctx context.Context,
	req sttprovider.TranscriptionRequest,
	onPartial func(sttprovider.TranscriptionResult) error,
) (sttprovider.TranscriptionResult, error) {
	httpReq, err := p.newTranscriptionRequest(ctx, req, true)
	if err != nil {
		return sttprovider.TranscriptionResult{}, err
	}
	resp, err := p.Client.Do(httpReq)
	if err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: parakeet sidecar unreachable: %v", sttprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, readErr)
		}
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: parakeet transcription failed: %s", sttprovider.ErrUnavailable, errorDetail(resp.StatusCode, respBody))
	}

	var final sttprovider.TranscriptionResult
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: invalid streaming event: %v", sttprovider.ErrUnavailable, err)
		}
		switch event.Type {
		case "transcript.text.delta":
			partial := sttprovider.TranscriptionResult{Text: event.Delta, ProviderID: id}
			final = partial
			if onPartial != nil {
				if err := onPartial(partial); err != nil {
					return sttprovider.TranscriptionResult{}, err
				}
			}
		case "transcript.text.done":
			final = sttprovider.TranscriptionResult{Text: event.Text, ProviderID: id}
		case "error":
			return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: %s", sttprovider.ErrUnavailable, event.Error.Message)
		}
	}
	if err := scanner.Err(); err != nil {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: streaming response: %v", sttprovider.ErrUnavailable, err)
	}
	if strings.TrimSpace(final.Text) == "" {
		return sttprovider.TranscriptionResult{}, fmt.Errorf("%w: streaming response contained no transcript", sttprovider.ErrUnavailable)
	}
	return final, nil
}

func (p Provider) newTranscriptionRequest(
	ctx context.Context,
	req sttprovider.TranscriptionRequest,
	stream bool,
) (*http.Request, error) {
	if len(req.Audio) == 0 {
		return nil, fmt.Errorf("%w: audio is required", sttprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return nil, fmt.Errorf("%w: lifecycle start failed: %v", sttprovider.ErrUnavailable, err)
		}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", id); err != nil {
		return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if language := strings.TrimSpace(req.Language); language != "" {
		if err := writer.WriteField("language", language); err != nil {
			return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
		}
	}
	if stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
		}
	}
	filename := strings.TrimSpace(req.Filename)
	if filename == "" {
		filename = "audio.wav"
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if _, err := part.Write(req.Audio); err != nil {
		return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sttprovider.ErrUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())
	return httpReq, nil
}

func errorDetail(status int, body []byte) string {
	var parsed errorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return fmt.Sprintf("status %d: %s", status, strings.TrimSpace(string(body)))
}
