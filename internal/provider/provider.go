package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

var (
	ErrUnavailable       = errors.New("audio provider unavailable")
	ErrUnsupportedFormat = errors.New("audio format unsupported")
	ErrInvalidRequest    = errors.New("invalid audio request")
)

type SpeechRequest struct {
	Model           string          `json:"model"`
	Input           string          `json:"input"`
	Voice           string          `json:"voice"`
	Language        string          `json:"language"`
	ResponseFormat  string          `json:"response_format"`
	Speed           float64         `json:"speed"`
	ProviderOptions json.RawMessage `json:"provider_options,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
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

// LanguageCatalog is an optional provider capability used by the discovery
// endpoint. Voices(ctx, language) remains the source of truth for validating
// a request; Languages reports the provider's complete language catalog when
// it can do so without a language filter.
type LanguageCatalog interface {
	Languages(ctx context.Context) ([]string, error)
}

// AutoLanguageProvider marks providers that accept the API's special
// language value "auto". The value means provider-defined automatic/default
// language handling; it is distinct from selecting a concrete BCP-47 tag.
type AutoLanguageProvider interface {
	SupportsAutoLanguage() bool
}

// Lifecycle is an optional interface for providers that manage an expensive
// resource (e.g. a GPU-resident model). The queue manager calls Warm before
// dispatching the first job to a cold provider, and Idle after the
// provider's queue has been empty for a configured duration. Providers that
// don't need this (e.g. stateless subprocess-based providers) simply don't
// implement it.
type Lifecycle interface {
	Warm(ctx context.Context) error
	Idle(ctx context.Context) error
}

// StreamMeta describes a streaming response, computed before any audio
// bytes are written.
type StreamMeta struct {
	ProviderID  string
	Model       string
	Voice       string
	Format      string
	ContentType string
}

// Streamer is an optional interface for providers that can write audio
// progressively instead of buffering the full result. SynthesizeStream must
// compute StreamMeta and call onHeader exactly once before writing any bytes
// to w — voice/format selection doesn't depend on the actual audio bytes, so
// this can happen synchronously up front. Providers that don't implement
// this (e.g. a diffusion model that only produces a complete waveform) are
// simply not eligible for streaming; callers fall back to Synthesize.
type Streamer interface {
	SynthesizeStream(ctx context.Context, req SpeechRequest, onHeader func(StreamMeta), w io.Writer) error
}
