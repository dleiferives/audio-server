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
	"github.com/dleiferives/audio-server/internal/queue"
)

const (
	defaultMaxInputChars  = 5000
	defaultRequestTimeout = 30 * time.Second
)

type Config struct {
	Providers       []provider.Provider
	DefaultProvider string
	APIKey          string
	MaxInputChars   int
	RequestTimeout  time.Duration
	// Queue schedules and executes synthesis jobs. Required.
	Queue *queue.Manager
	// StreamWorkers bounds concurrent streaming requests per provider ID.
	// This is independent of Queue's worker pools — streaming bypasses the
	// queue entirely since it's tied to one live connection rather than a
	// poll-able job. Providers not listed (or with a value <= 0) get 1.
	StreamWorkers map[string]int
}

type Server struct {
	providers       map[string]provider.Provider
	defaultProvider string
	apiKey          string
	maxInputChars   int
	requestTimeout  time.Duration
	queue           *queue.Manager
	streamSem       map[string]chan struct{}
}

func New(cfg Config) (*Server, error) {
	if len(cfg.Providers) == 0 {
		return nil, errors.New("at least one audio provider is required")
	}
	if cfg.Queue == nil {
		return nil, errors.New("a queue manager is required")
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
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	streamSem := make(map[string]chan struct{}, len(providers))
	for id := range providers {
		workers := cfg.StreamWorkers[id]
		if workers <= 0 {
			workers = 1
		}
		streamSem[id] = make(chan struct{}, workers)
	}
	return &Server{
		providers:       providers,
		defaultProvider: cfg.DefaultProvider,
		apiKey:          cfg.APIKey,
		maxInputChars:   cfg.MaxInputChars,
		requestTimeout:  cfg.RequestTimeout,
		queue:           cfg.Queue,
		streamSem:       streamSem,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/audio/voices", s.auth(s.voices))
	mux.HandleFunc("POST /v1/audio/speech", s.auth(s.speech))
	mux.HandleFunc("POST /v1/audio/jobs", s.auth(s.createJob))
	mux.HandleFunc("GET /v1/audio/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("GET /v1/audio/jobs/{id}/audio", s.auth(s.getJobAudio))
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
	req, p, ok := s.decodeAndRoute(w, r)
	if !ok {
		return
	}

	if req.Stream {
		if streamer, ok := p.(provider.Streamer); ok {
			s.streamSpeech(w, r, req, p.ID(), streamer)
			return
		}
		// Provider can't stream — fall through to the buffered path below.
	}

	job, err := s.queue.Submit(p.ID(), req)
	if err != nil {
		writeProviderError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	finished, err := s.queue.Wait(ctx, job.ID)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	if finished.Status == queue.StatusFailed {
		writeProviderError(w, finished.Err)
		return
	}
	writeAudio(w, finished.Result)
}

// streamSpeech writes audio progressively as the provider produces it.
// Unlike the buffered/job paths, this ties execution to the live HTTP
// connection: if the client disconnects or the request times out, the
// underlying synthesis is genuinely cancelled — there's no queued job or
// other consumer waiting on this output. It also bypasses queue.Manager
// entirely, gated instead by a small per-provider semaphore sized from
// Config.StreamWorkers.
func (s *Server) streamSpeech(w http.ResponseWriter, r *http.Request, req provider.SpeechRequest, providerID string, streamer provider.Streamer) {
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	sem := s.streamSem[providerID]
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		writeProviderError(w, ctx.Err())
		return
	}

	fw := &flushWriter{w: w}
	err := streamer.SynthesizeStream(ctx, req, func(meta provider.StreamMeta) {
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-TTS-Provider", meta.ProviderID)
		w.Header().Set("X-TTS-Model", meta.Model)
		w.Header().Set("X-TTS-Voice", meta.Voice)
		w.Header().Set("X-TTS-Format", meta.Format)
		w.WriteHeader(http.StatusOK)
	}, fw)
	if err != nil && !fw.headerWritten {
		// Nothing sent yet — still safe to write a normal error response.
		writeProviderError(w, err)
	}
	// If headers/bytes were already flushed, there's nothing left to do on
	// error: the client already has a 200 with a truncated body. Chunked
	// transfer encoding surfaces this as a short read on the client side.
}

type flushWriter struct {
	w             http.ResponseWriter
	headerWritten bool
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	fw.headerWritten = true
	n, err := fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	req, p, ok := s.decodeAndRoute(w, r)
	if !ok {
		return
	}
	if req.Stream {
		writeError(w, http.StatusBadRequest, errors.New("stream is not supported for async jobs; use POST /v1/audio/speech"))
		return
	}

	job, err := s.queue.Submit(p.ID(), req)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.jobStatusBody(job.ID))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.queue.Get(id); !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown job id"))
		return
	}
	writeJSON(w, http.StatusOK, s.jobStatusBody(id))
}

func (s *Server) getJobAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.queue.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown job id"))
		return
	}
	switch job.Status {
	case queue.StatusSucceeded:
		writeAudio(w, job.Result)
	case queue.StatusFailed:
		writeError(w, http.StatusNotFound, fmt.Errorf("job failed: %v", job.Err))
	default:
		writeError(w, http.StatusConflict, errors.New("job is not finished yet"))
	}
}

func (s *Server) jobStatusBody(id string) map[string]any {
	job, _ := s.queue.Get(id)
	body := map[string]any{
		"id":             job.ID,
		"status":         job.Status,
		"provider":       job.ProviderID,
		"created_at":     job.CreatedAt,
		"queue_position": 0,
	}
	if job.Status == queue.StatusQueued {
		body["queue_position"] = s.queue.Position(id)
	}
	if !job.StartedAt.IsZero() {
		body["started_at"] = job.StartedAt
	}
	if !job.FinishedAt.IsZero() {
		body["finished_at"] = job.FinishedAt
	}
	if job.Status == queue.StatusFailed && job.Err != nil {
		body["error"] = job.Err.Error()
	}
	return body
}

// decodeAndRoute decodes and validates the speech request body and resolves
// the provider it should run on. On failure it writes the error response and
// returns ok=false.
func (s *Server) decodeAndRoute(w http.ResponseWriter, r *http.Request) (provider.SpeechRequest, provider.Provider, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxInputChars*4+4096))
	defer r.Body.Close()

	var req provider.SpeechRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return provider.SpeechRequest{}, nil, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return provider.SpeechRequest{}, nil, false
	}
	if err := s.validateSpeech(req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return provider.SpeechRequest{}, nil, false
	}
	req.ResponseFormat = normalizedFormat(req.ResponseFormat)

	p, ok := s.providerFor(req.Model, req.Language)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown audio model"))
		return provider.SpeechRequest{}, nil, false
	}
	return req, p, true
}

func writeAudio(w http.ResponseWriter, result provider.SpeechResult) {
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
