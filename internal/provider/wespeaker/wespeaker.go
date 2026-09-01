// Package wespeaker connects to the persistent official WeSpeaker C++ ONNX runtime.
package wespeaker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
)

const ID = "wespeaker"

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
		return fmt.Errorf("%w: WeSpeaker sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: WeSpeaker health status %d", analysisprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

func (p Provider) Embed(ctx context.Context, input analysisprovider.AudioRequest) (analysisprovider.EmbeddingResult, error) {
	if len(input.Audio) == 0 {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: audio is required", analysisprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: lifecycle start failed: %v", analysisprovider.ErrUnavailable, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/embed", bytes.NewReader(input.Audio))
	if err != nil {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := p.Client.Do(req)
	if err != nil {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: WeSpeaker sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	var parsed struct {
		Model      string    `json:"model"`
		Duration   float64   `json:"duration"`
		Dimensions int       `json:"dimensions"`
		Embedding  []float32 `json:"embedding"`
		Error      struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: invalid WeSpeaker response: %v", analysisprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: WeSpeaker embedding failed: %s", analysisprovider.ErrUnavailable, parsed.Error.Message)
	}
	if parsed.Dimensions <= 0 || len(parsed.Embedding) != parsed.Dimensions {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: WeSpeaker returned inconsistent embedding dimensions", analysisprovider.ErrUnavailable)
	}
	return analysisprovider.EmbeddingResult{
		ProviderID: ID, Model: parsed.Model, Duration: parsed.Duration, Embedding: parsed.Embedding,
	}, nil
}
