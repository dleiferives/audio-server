// Package htdemucsdnr exposes a DnR-trained HTDemucs cinematic separator
// (dialogue/music/effects) via a Python sidecar running the real PyTorch
// demucs package. audio.cpp's native HTDemucs C++ port can't run this
// checkpoint — it has bottom_channels=0 (the official "skip the bottleneck
// projection" mode), which audio.cpp's implementation doesn't handle yet
// (see tmp/TOOD.md) — so this bypasses that entirely by running the
// checkpoint through the same demucs package it was trained with.
package htdemucsdnr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
)

const ID = "htdemucs-dnr"

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
		return err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: HTDemucs-DnR sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTDemucs-DnR health status %d", analysisprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

func (p Provider) Separate(ctx context.Context, input analysisprovider.AudioRequest) (analysisprovider.SeparationResult, error) {
	if len(input.Audio) == 0 {
		return analysisprovider.SeparationResult{}, fmt.Errorf("%w: audio is required", analysisprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return analysisprovider.SeparationResult{}, fmt.Errorf("%w: lifecycle start failed: %v", analysisprovider.ErrUnavailable, err)
		}
	}
	tmp, err := os.CreateTemp("", "audio-server-separate-*.wav")
	if err != nil {
		return analysisprovider.SeparationResult{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(input.Audio); err != nil {
		tmp.Close()
		return analysisprovider.SeparationResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return analysisprovider.SeparationResult{}, err
	}
	payload, _ := json.Marshal(map[string]any{"model": ID, "request": map[string]any{"audio": path}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/tasks/run", bytes.NewReader(payload))
	if err != nil {
		return analysisprovider.SeparationResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return analysisprovider.SeparationResult{}, fmt.Errorf("%w: HTDemucs-DnR sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return analysisprovider.SeparationResult{}, err
	}
	var parsed struct {
		Outputs []struct {
			ID    string `json:"id"`
			Audio string `json:"audio"`
		} `json:"named_audio_outputs"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return analysisprovider.SeparationResult{}, fmt.Errorf("%w: invalid HTDemucs-DnR response: %v", analysisprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return analysisprovider.SeparationResult{}, fmt.Errorf("%w: HTDemucs-DnR separation failed: %s", analysisprovider.ErrUnavailable, parsed.Error.Message)
	}
	result := analysisprovider.SeparationResult{ProviderID: ID, Outputs: make(map[string][]byte, len(parsed.Outputs))}
	for _, output := range parsed.Outputs {
		decoded, err := base64.StdEncoding.DecodeString(output.Audio)
		if err != nil {
			return analysisprovider.SeparationResult{}, fmt.Errorf("%w: invalid %s stem: %v", analysisprovider.ErrUnavailable, output.ID, err)
		}
		result.Outputs[output.ID] = decoded
	}
	if len(result.Outputs) == 0 {
		return analysisprovider.SeparationResult{}, fmt.Errorf("%w: HTDemucs-DnR returned no stems", analysisprovider.ErrUnavailable)
	}
	return result, nil
}
