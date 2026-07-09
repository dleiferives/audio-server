package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dleiferives/audio-server/internal/provider"
)

const (
	defaultMaxInputChars  = 5000
	defaultMaxConcurrency = 2
	defaultRequestTimeout = 30 * time.Second
)

type Config struct {
	Providers       []provider.Provider
	DefaultProvider string
	APIKey          string
	MaxInputChars   int
	MaxConcurrency  int
	RequestTimeout  time.Duration
}

type Server struct {
	providers       map[string]provider.Provider
	defaultProvider string
	apiKey          string
	maxInputChars   int
	requestTimeout  time.Duration
	sem             chan struct{}
}

func New(cfg Config) (*Server, error) {
	if len(cfg.Providers) == 0 {
		return nil, errors.New("at least one audio provider is required")
	}
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if p == nil {
			return nil, errors.New("audio provider is nil")
		}
		id := strings.TrimSpace(p.ID())
		if id == "" {
			return nil, errors.New("audio provider id is empty")
		}
		providers[id] = p
	}
	cfg.DefaultProvider = strings.TrimSpace(cfg.DefaultProvider)
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = cfg.Providers[0].ID()
	}
	if _, ok := providers[cfg.DefaultProvider]; !ok {
		return nil, fmt.Errorf("default audio provider %q is not registered", cfg.DefaultProvider)
	}
	if cfg.MaxInputChars <= 0 {
		cfg.MaxInputChars = defaultMaxInputChars
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return &Server{
		providers:       providers,
		defaultProvider: cfg.DefaultProvider,
		apiKey:          cfg.APIKey,
		maxInputChars:   cfg.MaxInputChars,
		requestTimeout:  cfg.RequestTimeout,
		sem:             make(chan struct{}, cfg.MaxConcurrency),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/audio/voices", s.auth(s.voices))
	mux.HandleFunc("POST /v1/audio/speech", s.auth(s.speech))
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := http.StatusOK
	checks := make(map[string]string, len(s.providers))
	for id, p := range s.providers {
		if err := p.Health(ctx); err != nil {
			status = http.StatusServiceUnavailable
			checks[id] = err.Error()
			continue
		}
		checks[id] = "ok"
	}
	body := map[string]any{"status": "ok", "providers": checks}
	if status != http.StatusOK {
		body["status"] = "unhealthy"
	}
	writeJSON(w, status, body)
}

func (s *Server) voices(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFor(r.URL.Query().Get("provider"), r.URL.Query().Get("language"))
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown audio provider"))
		return
	}
	voices, err := p.Voices(r.Context(), r.URL.Query().Get("language"))
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": voices})
}

func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxInputChars*4+4096))
	defer r.Body.Close()

	var req provider.SpeechRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}
	if err := s.validateSpeech(req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ResponseFormat = normalizedFormat(req.ResponseFormat)

	p, ok := s.providerFor(req.Model, req.Language)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown audio model"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	if err := s.acquire(ctx); err != nil {
		writeProviderError(w, err)
		return
	}
	defer s.release()

	result, err := p.Synthesize(ctx, req)
	if err != nil {
		writeProviderError(w, err)
		return
	}

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-TTS-Provider", result.ProviderID)
	w.Header().Set("X-TTS-Model", result.Model)
	w.Header().Set("X-TTS-Voice", result.Voice)
	w.Header().Set("X-TTS-Format", result.Format)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Audio)
}

func (s *Server) validateSpeech(req provider.SpeechRequest) error {
	if strings.TrimSpace(req.Input) == "" {
		return errors.New("input is required")
	}
	if utf8.RuneCountInString(req.Input) > s.maxInputChars {
		return fmt.Errorf("input is too long; max %d characters", s.maxInputChars)
	}
	format := normalizedFormat(req.ResponseFormat)
	if format != "mp3" && format != "wav" {
		return errors.New("response_format must be mp3 or wav")
	}
	if req.Speed != 0 && (req.Speed < 0.25 || req.Speed > 4.0) {
		return errors.New("speed must be between 0.25 and 4.0")
	}
	return nil
}

// languageProviders routes requests to a specific provider by language when
// no model is given, for providers that only serve one language (e.g.
// OmniVoice serves Greek). Keyed by normalized BCP-47 language.
var languageProviders = map[string]string{
	"el": "omnivoice",
}

func (s *Server) providerFor(model, language string) (provider.Provider, bool) {
	model = strings.TrimSpace(model)
	switch model {
	case "", "auto", "tts-1", "tts-1-hd", "gpt-4o-mini-tts":
		if id, ok := languageProviders[normalizeLanguage(language)]; ok {
			if p, ok := s.providers[id]; ok {
				return p, true
			}
		}
		return s.providers[s.defaultProvider], true
	default:
		p, ok := s.providers[model]
		return p, ok
	}
}

func normalizeLanguage(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	return strings.ReplaceAll(language, "_", "-")
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() {
	<-s.sem
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	if s.apiKey == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r.Header.Get("Authorization"))
		if got == "" {
			got = r.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("invalid audio API key"))
			return
		}
		next(w, r)
	}
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

func normalizedFormat(format string) string {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		return "mp3"
	}
	return format
}

func writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, err)
	case errors.Is(err, provider.ErrUnsupportedFormat), errors.Is(err, provider.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, provider.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
