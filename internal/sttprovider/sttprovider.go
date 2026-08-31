// Package sttprovider defines the speech-to-text provider interface. It's
// deliberately separate from internal/provider (text-to-speech): the two
// directions have different request/result shapes and, for now, different
// lifecycle needs — see docs/providers.md for why STT doesn't route through
// internal/queue the way TTS does.
package sttprovider

import (
	"context"
	"errors"
)

var (
	ErrUnavailable       = errors.New("stt provider unavailable")
	ErrUnsupportedFormat = errors.New("audio format unsupported")
	ErrInvalidRequest    = errors.New("invalid transcription request")
)

type TranscriptionRequest struct {
	Audio    []byte
	Filename string
	Language string
	Model    string
}

type TranscriptionResult struct {
	Text       string
	Language   string
	Duration   float64
	ProviderID string
}

// AudioFormat describes the canonical input a provider requires. Empty or
// zero-valued fields mean that the provider does not constrain that property.
type AudioFormat struct {
	Container  string
	Codec      string
	SampleRate int
	Channels   int
}

// AudioRequirementsProvider is implemented by providers that require uploads
// to be normalized before dispatch. Providers that accept arbitrary encoded
// audio should not implement this interface.
type AudioRequirementsProvider interface {
	AudioRequirements() AudioFormat
}

type NormalizedAudio struct {
	Audio     []byte
	Converted bool
}

// AudioNormalizer converts arbitrary uploaded audio into a provider's required
// input format. Implementations should return the original bytes unchanged
// when they already match the requirement.
type AudioNormalizer interface {
	NormalizeAudio(ctx context.Context, audio []byte, format AudioFormat) (NormalizedAudio, error)
}

type Provider interface {
	ID() string
	Health(ctx context.Context) error
	Transcribe(ctx context.Context, req TranscriptionRequest) (TranscriptionResult, error)
}

// StreamingProvider emits cumulative partial transcription snapshots while a
// request is being decoded, then returns the final result.
type StreamingProvider interface {
	TranscribeStream(ctx context.Context, req TranscriptionRequest, onPartial func(TranscriptionResult) error) (TranscriptionResult, error)
}

// OnDemandProvider is implemented by providers whose sidecar is intentionally
// stopped while idle and started by Transcribe. Health checks report an
// unreachable on-demand provider as cold rather than making the whole service
// unhealthy.
type OnDemandProvider interface {
	StartsOnDemand() bool
}
