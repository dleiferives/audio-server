package provider

import (
	"context"
	"errors"
)

var (
	ErrUnavailable       = errors.New("audio provider unavailable")
	ErrUnsupportedFormat = errors.New("audio format unsupported")
)

type SpeechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	Language       string  `json:"language"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

type SpeechResult struct {
	Audio       []byte
	ContentType string
	ProviderID  string
	Model       string
	Voice       string
	Format      string
}

type Voice struct {
	Provider string `json:"provider"`
	Voice    string `json:"voice"`
	Language string `json:"language"`
	Name     string `json:"name"`
	Gender   string `json:"gender,omitempty"`
}

type Provider interface {
	ID() string
	Health(ctx context.Context) error
	Voices(ctx context.Context, language string) ([]Voice, error)
	Synthesize(ctx context.Context, req SpeechRequest) (SpeechResult, error)
}
