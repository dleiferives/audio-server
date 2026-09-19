// Package pyannote connects to the pyannote.audio diarization HTTP sidecar
// (speaker/pyannote/server.py). Unlike Sortformer, pyannote's community-1
// pipeline diarizes and embeds speakers across an arbitrary-length clip in
// one pass, so this provider implements both Diarizer and Embedder.
package pyannote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
)

const ID = "pyannote"

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
		return fmt.Errorf("%w: pyannote sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: pyannote health status %d", analysisprovider.ErrUnavailable, resp.StatusCode)
	}
	return nil
}

type diarizeResponse struct {
	Model      string                         `json:"model"`
	Duration   float64                        `json:"duration"`
	SampleRate int                            `json:"sample_rate"`
	Turns      []analysisprovider.SpeakerTurn `json:"turns"`
	Speakers   map[string]struct {
		Embedding  []float32 `json:"embedding"`
		Dimensions int       `json:"dimensions"`
	} `json:"speakers"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p Provider) diarize(ctx context.Context, input analysisprovider.AudioRequest) (diarizeResponse, error) {
	if len(input.Audio) == 0 {
		return diarizeResponse{}, fmt.Errorf("%w: audio is required", analysisprovider.ErrInvalidRequest)
	}
	if p.StartFunc != nil {
		if err := p.StartFunc(); err != nil {
			return diarizeResponse{}, fmt.Errorf("%w: lifecycle start failed: %v", analysisprovider.ErrUnavailable, err)
		}
	}
	query := url.Values{}
	if input.NumSpeakers > 0 {
		query.Set("num_speakers", strconv.Itoa(input.NumSpeakers))
	}
	if input.MinSpeakers > 0 {
		query.Set("min_speakers", strconv.Itoa(input.MinSpeakers))
	}
	if input.MaxSpeakers > 0 {
		query.Set("max_speakers", strconv.Itoa(input.MaxSpeakers))
	}
	endpoint := p.BaseURL + "/diarize"
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(input.Audio))
	if err != nil {
		return diarizeResponse{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "audio/wav")
	resp, err := p.Client.Do(req)
	if err != nil {
		return diarizeResponse{}, fmt.Errorf("%w: pyannote sidecar unreachable: %v", analysisprovider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return diarizeResponse{}, fmt.Errorf("%w: %v", analysisprovider.ErrUnavailable, err)
	}
	var parsed diarizeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return diarizeResponse{}, fmt.Errorf("%w: invalid pyannote response: %v", analysisprovider.ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return diarizeResponse{}, fmt.Errorf("%w: pyannote diarization failed: %s", analysisprovider.ErrUnavailable, parsed.Error.Message)
	}
	return parsed, nil
}

// Diarize implements analysisprovider.Diarizer.
func (p Provider) Diarize(ctx context.Context, input analysisprovider.AudioRequest) (analysisprovider.DiarizationResult, error) {
	parsed, err := p.diarize(ctx, input)
	if err != nil {
		return analysisprovider.DiarizationResult{}, err
	}
	sampleRate := parsed.SampleRate
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	result := analysisprovider.DiarizationResult{ProviderID: ID, SampleRate: sampleRate, Turns: parsed.Turns}
	if len(parsed.Speakers) > 0 {
		result.SpeakerEmbeddings = make(map[string][]float32, len(parsed.Speakers))
		for speaker, emb := range parsed.Speakers {
			if emb.Dimensions > 0 && len(emb.Embedding) == emb.Dimensions {
				result.SpeakerEmbeddings[speaker] = emb.Embedding
			}
		}
		result.EmbeddingModel = parsed.Model
	}
	return result, nil
}

// Embed implements analysisprovider.Embedder by re-running diarization and
// returning the embedding for the speaker cluster whose samples were passed
// in. Callers that already have a pyannote DiarizationResult in hand should
// prefer its per-speaker embeddings directly; this exists so pyannote can
// still satisfy the generic Embedder contract for a single reference clip.
func (p Provider) Embed(ctx context.Context, input analysisprovider.AudioRequest) (analysisprovider.EmbeddingResult, error) {
	parsed, err := p.diarize(ctx, input)
	if err != nil {
		return analysisprovider.EmbeddingResult{}, err
	}
	if len(parsed.Speakers) == 0 {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: pyannote returned no speaker embeddings", analysisprovider.ErrUnavailable)
	}
	// A reference clip collected for one speaker should diarize to a single
	// dominant cluster; pick the speaker with the most turns.
	counts := make(map[string]int, len(parsed.Speakers))
	for _, turn := range parsed.Turns {
		counts[turn.SpeakerID]++
	}
	best := ""
	bestCount := -1
	for speaker := range parsed.Speakers {
		if counts[speaker] > bestCount {
			best = speaker
			bestCount = counts[speaker]
		}
	}
	emb := parsed.Speakers[best]
	if emb.Dimensions <= 0 || len(emb.Embedding) != emb.Dimensions {
		return analysisprovider.EmbeddingResult{}, fmt.Errorf("%w: pyannote returned inconsistent embedding dimensions", analysisprovider.ErrUnavailable)
	}
	return analysisprovider.EmbeddingResult{
		ProviderID: ID, Model: parsed.Model, Duration: parsed.Duration, Embedding: emb.Embedding,
	}, nil
}
