// Package sortformer exposes audio.cpp's native Sortformer diarizer.
package sortformer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
)

const ID = "sortformer"

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Provider struct {
	BaseURL   string
	Client    HTTPClient
	StartFunc func() error
}

func New(baseURL string, client HTTPClient) Provider {
	if client == nil {
		client = http.DefaultClient
	}
	return Provider{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), Client: client}
}

func (p Provider) ID() string { return ID }

func (p Provider) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: Sortformer sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: Sortformer health status %d", analysisprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

func (p Provider) Diarize(ctx context.Context, input analysisprovider.AudioRequest) (analysisprovider.DiarizationResult, error) {
	if len(input.Audio) == 0 {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: audio is required", analysisprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: lifecycle start failed: %v", analysisprovider.ErrUnavailable, err)
		}
	}
	tmp, err := os.CreateTemp("", "audio-server-diar-*.wav")
	if err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: temporary audio: %v", analysisprovider.ErrUnavailable, err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(input.Audio); err != nil {
		tmp.Close()
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: temporary audio: %v", analysisprovider.ErrUnavailable, err)
	}
	if err := tmp.Close(); err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: temporary audio: %v", analysisprovider.ErrUnavailable, err)
	}
	payload, err := json.Marshal(map[string]any{
		"model":   ID,
		"request": map[string]any{"audio": path},
	})
	if err != nil {
		return analysisprovider.DiarizationResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/tasks/run", bytes.NewReader(payload))
	if err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: Sortformer sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	var parsed struct {
		SpeakerTurns []analysisprovider.SpeakerTurn `json:"speaker_turns"`
		Error        struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: invalid Sortformer response: %v", analysisprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return analysisprovider.DiarizationResult{}, fmt.Errorf("%w: Sortformer diarization failed: %s", analysisprovider.ErrUnavailable, parsed.Error.Message)
	}
	return analysisprovider.DiarizationResult{ProviderID: ID, SampleRate: 16000, Turns: parsed.SpeakerTurns}, nil
}
