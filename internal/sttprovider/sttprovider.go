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
