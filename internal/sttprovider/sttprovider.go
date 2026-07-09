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
