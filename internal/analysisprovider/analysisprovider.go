// Package analysisprovider defines native speech-analysis provider contracts.
package analysisprovider

import (
	"context"
	"errors"
)

var (
	ErrUnavailable    = errors.New("analysis provider unavailable")
	ErrInvalidRequest = errors.New("invalid analysis request")
)

type AudioRequest struct {
	Audio    []byte
	Filename string

	// NumSpeakers/MinSpeakers/MaxSpeakers are optional hints for diarizers
	// that support constraining cluster count (e.g. pyannote). Zero means
	// unset/automatic. Diarizers that don't support this (e.g. Sortformer's
	// fixed local-speaker cap) simply ignore them.
	NumSpeakers int
	MinSpeakers int
	MaxSpeakers int
}

type SpeakerTurn struct {
	StartSample int64   `json:"start_sample"`
	EndSample   int64   `json:"end_sample"`
	SpeakerID   string  `json:"speaker_id"`
	Confidence  float64 `json:"confidence"`
}

type DiarizationResult struct {
	ProviderID string        `json:"provider"`
	SampleRate int           `json:"sample_rate"`
	Turns      []SpeakerTurn `json:"turns"`

	// SpeakerEmbeddings and EmbeddingModel are set by diarizers that compute
	// per-speaker embeddings as part of the same whole-clip pass (e.g.
	// pyannote), keyed by SpeakerTurn.SpeakerID. When present, callers can
	// use them directly instead of running a separate embedding provider
	// over collected per-speaker samples.
	SpeakerEmbeddings map[string][]float32 `json:"speaker_embeddings,omitempty"`
	EmbeddingModel    string               `json:"embedding_model,omitempty"`
}

type EmbeddingResult struct {
	ProviderID string    `json:"provider"`
	Model      string    `json:"model"`
	Duration   float64   `json:"duration"`
	Embedding  []float32 `json:"embedding"`
}

type SeparationResult struct {
	ProviderID string
	Outputs    map[string][]byte
}

type Diarizer interface {
	ID() string
	Health(context.Context) error
	Diarize(context.Context, AudioRequest) (DiarizationResult, error)
}

type Embedder interface {
	ID() string
	Health(context.Context) error
	Embed(context.Context, AudioRequest) (EmbeddingResult, error)
}

type Separator interface {
	ID() string
	Health(context.Context) error
	Separate(context.Context, AudioRequest) (SeparationResult, error)
}
