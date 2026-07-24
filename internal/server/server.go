package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/store"
	"github.com/dleiferives/audio-server/internal/sttprovider"
)

const (
	defaultMaxInputChars  = 5000
	defaultRequestTimeout = 30 * time.Second
	defaultMaxUploadBytes = 25 << 20 // 25MB, matching OpenAI's transcription upload limit
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

	// SttProviders is optional; the transcription endpoint returns 503 if
	// none are configured. Unlike TTS, STT doesn't go through internal/queue
	// today — see docs/providers.md for why.
	SttProviders       []sttprovider.Provider
	DefaultSttProvider string
	MaxUploadBytes     int

	// WebDir is an optional path to a directory of static files to serve at
	// the root. When set, GET requests not matching any API route are served
	// from this directory (e.g. an index.html frontend).
	WebDir string

	// AudioStore persists generated audio to disk when set.
	// Nil means audio is held in memory only.
	AudioStore *store.Store

	// AlignProvider handles forced alignment via MFA subprocess.
	// Nil when alignment is disabled.
	AlignProvider Aligner
}

// Aligner wraps the MFA-go alignment provider.
type Aligner interface {
	Align(ctx context.Context, audio []byte, transcript, language string) (any, error)
	Languages() []string
	HasLanguage(language string) bool
}

type Server struct {
	providers       map[string]provider.Provider
	defaultProvider string
	apiKey          string
	maxInputChars   int
	requestTimeout  time.Duration
	queue           *queue.Manager
	streamSem       map[string]chan struct{}

	sttProviders       map[string]sttprovider.Provider
	defaultSttProvider string
	maxUploadBytes     int
	webDir             string
	audioStore         *store.Store
	alignProvider      Aligner
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
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = defaultMaxUploadBytes
	}
	streamSem := make(map[string]chan struct{}, len(providers))
	for id := range providers {
		workers := cfg.StreamWorkers[id]
		if workers <= 0 {
			workers = 1
		}
		streamSem[id] = make(chan struct{}, workers)
	}

	sttProviders := make(map[string]sttprovider.Provider, len(cfg.SttProviders))
	for _, p := range cfg.SttProviders {
		if p == nil {
			return nil, errors.New("stt provider is nil")
		}
		id := strings.TrimSpace(p.ID())
		if id == "" {
			return nil, errors.New("stt provider id is empty")
		}
		sttProviders[id] = p
	}
	cfg.DefaultSttProvider = strings.TrimSpace(cfg.DefaultSttProvider)
	if cfg.DefaultSttProvider == "" && len(cfg.SttProviders) > 0 {
		cfg.DefaultSttProvider = cfg.SttProviders[0].ID()
	}
	if cfg.DefaultSttProvider != "" {
		if _, ok := sttProviders[cfg.DefaultSttProvider]; !ok {
			return nil, fmt.Errorf("default stt provider %q is not registered", cfg.DefaultSttProvider)
		}
	}

	return &Server{
		providers:          providers,
		defaultProvider:    cfg.DefaultProvider,
		apiKey:             cfg.APIKey,
		maxInputChars:      cfg.MaxInputChars,
		requestTimeout:     cfg.RequestTimeout,
		queue:              cfg.Queue,
		streamSem:          streamSem,
		sttProviders:       sttProviders,
		defaultSttProvider: cfg.DefaultSttProvider,
		maxUploadBytes:     cfg.MaxUploadBytes,
		webDir:             cfg.WebDir,
		audioStore:         cfg.AudioStore,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/audio/capabilities", s.auth(s.capabilities))
	mux.HandleFunc("GET /v1/audio/voices", s.auth(s.voices))
	mux.HandleFunc("POST /v1/audio/speech", s.auth(s.speech))
	mux.HandleFunc("POST /v1/audio/jobs", s.auth(s.createJob))
	mux.HandleFunc("GET /v1/audio/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("GET /v1/audio/jobs/{id}/audio", s.auth(s.getJobAudio))
	mux.HandleFunc("GET /v1/audio/jobs/{id}/stream", s.auth(s.getJobStream))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.auth(s.transcriptions))
	if s.webDir != "" {
		fs := http.FileServer(http.Dir(s.webDir))
		mux.Handle("GET /", fs)
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := http.StatusOK
	checks := make(map[string]string, len(s.providers)+len(s.sttProviders))
	for id, p := range s.providers {
		if err := p.Health(ctx); err != nil {
			if _, lifecycleManaged := p.(provider.Lifecycle); lifecycleManaged {
				// Lifecycle-managed providers are allowed to be cold: their
				// sidecar is started by the queue when a job reaches the shared
				// resource, rather than being required to stay up for /healthz.
				checks[id] = "cold"
				continue
			}
			status = http.StatusServiceUnavailable
			checks[id] = err.Error()
			continue
		}
		checks[id] = "ok"
	}
	for id, p := range s.sttProviders {
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
	language := normalizeLanguage(r.URL.Query().Get("language"))
	if language == "auto" && !supportsAutoLanguage(p) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("provider %q does not support language auto", p.ID()))
		return
	}
	voices, err := p.Voices(r.Context(), language)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": voices})
}

type providerCapabilities struct {
	Provider  string           `json:"provider"`
	Languages []string         `json:"languages"`
	Voices    []provider.Voice `json:"voices"`
}

// capabilities exposes the complete TTS catalog known by this audio-server.
// Clients can use the language list to populate a language picker, then call
// /v1/audio/voices with provider+language to get only compatible voices.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	ids := make([]string, 0, len(s.providers))
	for id := range s.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	allLanguages := make(map[string]bool)
	providers := make([]providerCapabilities, 0, len(ids))
	for _, id := range ids {
		p := s.providers[id]
		voices, err := p.Voices(r.Context(), "")
		if err != nil {
			writeProviderError(w, err)
			return
		}
		languages, err := providerLanguages(r.Context(), p, voices)
		if err != nil {
			writeProviderError(w, err)
			return
		}
		for _, language := range languages {
			allLanguages[language] = true
		}
		if autoProvider, ok := p.(provider.AutoLanguageProvider); ok && autoProvider.SupportsAutoLanguage() {
			languages = appendLanguage(languages, "auto")
			allLanguages["auto"] = true
		}
		providers = append(providers, providerCapabilities{
			Provider:  id,
			Languages: languages,
			Voices:    voices,
		})
	}

	languages := make([]string, 0, len(allLanguages))
	for language := range allLanguages {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	writeJSON(w, http.StatusOK, map[string]any{
		"languages": languages,
		"providers": providers,
	})
}

func providerLanguages(ctx context.Context, p provider.Provider, voices []provider.Voice) ([]string, error) {
	if catalog, ok := p.(provider.LanguageCatalog); ok {
		languages, err := catalog.Languages(ctx)
		if err != nil {
			return nil, err
		}
		return normalizedLanguages(languages), nil
	}

	seen := make(map[string]bool)
	var languages []string
	for _, voice := range voices {
		for _, language := range strings.Split(voice.Language, "/") {
			language = normalizeLanguage(language)
			if language != "" && !seen[language] {
				seen[language] = true
				languages = append(languages, language)
			}
		}
	}
	sort.Strings(languages)
	return languages, nil
}

func normalizedLanguages(languages []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(languages))
	for _, language := range languages {
		language = normalizeLanguage(language)
		if language != "" && !seen[language] {
			seen[language] = true
			result = append(result, language)
		}
	}
	sort.Strings(result)
	return result
}

func appendLanguage(languages []string, language string) []string {
	if language = normalizeLanguage(language); language == "" {
		return languages
	}
	for _, existing := range languages {
		if existing == language {
			return languages
		}
	}
	result := append(append([]string(nil), languages...), language)
	sort.Strings(result)
	return result
}

func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	req, p, ok := s.decodeAndRoute(w, r)
	if !ok {
		return
	}

	if req.Stream {
		if streamer, ok := p.(provider.Streamer); ok {
			if _, lifecycleManaged := p.(provider.Lifecycle); lifecycleManaged {
				// Queue the stream so lifecycle warm-up and shared-resource
				// handoff happen before the first response bytes are written.
				s.queuedStreamSpeech(w, r, req, p.ID(), streamer)
				return
			}
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

	ctx := r.Context()
	if s.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.requestTimeout)
		defer cancel()
	}
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

// queuedStreamSpeech keeps a lifecycle-managed stream live for the caller,
// while making the queue own provider warm-up and shared-resource scheduling.
// The provider's SSE/audio chunks are written only after its job reaches the
// front of the queue and its model is warm.
func (s *Server) queuedStreamSpeech(w http.ResponseWriter, r *http.Request, req provider.SpeechRequest, providerID string, streamer provider.Streamer) {
	ctx := r.Context()
	if s.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.requestTimeout)
		defer cancel()
	}

	var headerWritten atomic.Bool
	fw := &flushWriter{w: w}
	job, err := s.queue.SubmitStream(providerID, req, ctx, streamer, func(id string) {
		w.Header().Set("X-Job-ID", id)
	}, func(meta provider.StreamMeta) {
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-TTS-Provider", meta.ProviderID)
		w.Header().Set("X-TTS-Model", meta.Model)
		w.Header().Set("X-TTS-Voice", meta.Voice)
		w.Header().Set("X-TTS-Format", meta.Format)
		w.Header().Set("Transfer-Encoding", "chunked")
		headerWritten.Store(true)
		w.WriteHeader(http.StatusOK)
		fw.Flush()
	}, fw)
	if err != nil {
		writeProviderError(w, err)
		return
	}

	finished, err := s.queue.Wait(ctx, job.ID)
	if err != nil {
		if !headerWritten.Load() {
			writeProviderError(w, err)
		}
		return
	}
	if finished.Status == queue.StatusFailed {
		if !headerWritten.Load() {
			writeProviderError(w, finished.Err)
		}
		return
	}
	if s.audioStore != nil {
		_ = s.audioStore.Write(job.ID, finished.Result.Audio)
	}
}

// streamSpeech writes audio progressively as the provider produces it.
// Unlike the buffered/job paths, this ties execution to the live HTTP
// connection: if the client disconnects or the request times out, the
// underlying synthesis is genuinely cancelled — there's no queued job or
// other consumer waiting on this output. It also bypasses queue.Manager
// entirely, gated instead by a small per-provider semaphore sized from
// Config.StreamWorkers.
func (s *Server) streamSpeech(w http.ResponseWriter, r *http.Request, req provider.SpeechRequest, providerID string, streamer provider.Streamer) {
	ctx := r.Context()
	if s.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.requestTimeout)
		defer cancel()
	}

	sem := s.streamSem[providerID]
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		writeProviderError(w, ctx.Err())
		return
	}

	job := s.queue.NewRunningJob(providerID, req)
	w.Header().Set("X-Job-ID", job.ID)

	fw := &flushWriter{w: w}
	var buf bytes.Buffer
	mw := io.MultiWriter(fw, &buf)

	chunkW := &chunkCounter{w: mw, jobID: job.ID, queue: s.queue}
	err := streamer.SynthesizeStream(ctx, req, func(meta provider.StreamMeta) {
		w.Header().Set("Content-Type", meta.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-TTS-Provider", meta.ProviderID)
		w.Header().Set("X-TTS-Model", meta.Model)
		w.Header().Set("X-TTS-Voice", meta.Voice)
		w.Header().Set("X-TTS-Format", meta.Format)
		w.Header().Set("X-Job-ID", job.ID)
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
	}, chunkW)
	if err != nil {
		if !fw.headerWritten {
			s.queue.FailJob(job.ID, err)
			writeProviderError(w, err)
		} else {
			s.queue.FailJob(job.ID, err)
		}
		return
	}
	fw.Flush()
	if s.audioStore != nil {
		s.audioStore.Write(job.ID, buf.Bytes())
	}
	s.queue.CompleteJob(job.ID, provider.SpeechResult{
		Audio:       buf.Bytes(),
		ContentType: "audio/pcm",
		ProviderID:  providerID,
		Model:       providerID,
		Voice:       req.Voice,
		Format:      "pcm",
	})
}

// getJobStream streams live audio for a running job or replays from disk
// for a completed job.
func (s *Server) getJobStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.queue.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown job id"))
		return
	}

	switch job.Status {
	case queue.StatusRunning:
		if job.StreamBus == nil {
			writeError(w, http.StatusConflict, errors.New("job is not streaming"))
			return
		}
		w.Header().Set("Content-Type", "audio/pcm")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for chunk := range job.StreamBus {
			w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	case queue.StatusSucceeded:
		var audio []byte
		if s.audioStore != nil {
			var err error
			audio, err = s.audioStore.Read(id)
			if err != nil {
				writeError(w, http.StatusNotFound, errors.New("audio file not found"))
				return
			}
		} else {
			audio = job.Result.Audio
		}
		w.Header().Set("Content-Type", "audio/pcm")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(audio)))
		w.WriteHeader(http.StatusOK)
		w.Write(audio)
	default:
		writeError(w, http.StatusConflict, errors.New("job is not finished yet"))
	}
}

type chunkCounter struct {
	w     io.Writer
	jobID string
	queue *queue.Manager
}

func (c *chunkCounter) Write(p []byte) (int, error) {
	c.queue.BumpChunk(c.jobID, p)
	return c.w.Write(p)
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

func (fw *flushWriter) Flush() {
	if f, ok := fw.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	req, p, ok := s.decodeAndRoute(w, r)
	if !ok {
		return
	}
	if req.Stream {
		if streamer, ok := p.(provider.Streamer); ok {
			if _, lifecycleManaged := p.(provider.Lifecycle); !lifecycleManaged {
				job := s.queue.NewRunningJob(p.ID(), req)
				go func() {
					var buf bytes.Buffer
					cw := &chunkCounter{w: &buf, jobID: job.ID, queue: s.queue}
					err := streamer.SynthesizeStream(context.Background(), req, func(meta provider.StreamMeta) {}, cw)
					if err != nil {
						s.queue.FailJob(job.ID, err)
						return
					}
					if s.audioStore != nil {
						s.audioStore.Write(job.ID, buf.Bytes())
					}
					s.queue.CompleteJob(job.ID, provider.SpeechResult{
						Audio:       buf.Bytes(),
						ContentType: "audio/pcm",
						ProviderID:  p.ID(),
						Model:       p.ID(),
						Voice:       req.Voice,
						Format:      "pcm",
					})
				}()
				writeJSON(w, http.StatusAccepted, s.jobStatusBody(job.ID))
				return
			}
		}
		// Lifecycle-managed streamers use the buffered queue path below so
		// warm-up and exclusive-resource scheduling remain centralized.
		if _, lifecycleManaged := p.(provider.Lifecycle); !lifecycleManaged {
			writeError(w, http.StatusBadRequest, errors.New("streaming not supported by this provider"))
			return
		}
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
	if job.Status == queue.StatusRunning && job.StreamChunks > 0 {
		body["stream_chunks"] = job.StreamChunks
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
	if err := s.resolveVoice(r.Context(), p, &req); err != nil {
		writeProviderError(w, err)
		return provider.SpeechRequest{}, nil, false
	}
	return req, p, true
}

// transcriptions handles POST /v1/audio/transcriptions, an OpenAI-compatible
// multipart/form-data endpoint: a "file" field with the audio, optional
// "model" (provider ID; "whisper-1" and blank map to the default STT
// provider), "language", and "response_format" ("json", the default, or
// "text").
func (s *Server) transcriptions(w http.ResponseWriter, r *http.Request) {
	if len(s.sttProviders) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("no speech-to-text providers configured"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxUploadBytes))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("file is required"))
		return
	}
	defer file.Close()
	audio, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	model := r.FormValue("model")
	responseFormat := r.FormValue("response_format")
	if strings.TrimSpace(responseFormat) == "" {
		responseFormat = "json"
	}
	if responseFormat != "json" && responseFormat != "text" {
		writeError(w, http.StatusBadRequest, errors.New("response_format must be json or text"))
		return
	}

	p, ok := s.sttProviderFor(model)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown transcription model"))
		return
	}

	ctx := r.Context()
	if s.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.requestTimeout)
		defer cancel()
	}
	result, err := p.Transcribe(ctx, sttprovider.TranscriptionRequest{
		Audio:    audio,
		Filename: header.Filename,
		Language: r.FormValue("language"),
		Model:    model,
	})
	if err != nil {
		writeSttProviderError(w, err)
		return
	}

	if responseFormat == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(result.Text))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": result.Text})
}

func (s *Server) sttProviderFor(model string) (sttprovider.Provider, bool) {
	model = strings.TrimSpace(model)
	switch model {
	case "", "whisper-1", "auto":
		p, ok := s.sttProviders[s.defaultSttProvider]
		return p, ok
	default:
		p, ok := s.sttProviders[model]
		return p, ok
	}
}

func writeSttProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, err)
	case errors.Is(err, sttprovider.ErrUnsupportedFormat), errors.Is(err, sttprovider.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, sttprovider.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
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

var supportedFormats = map[string]bool{
	"mp3":  true,
	"wav":  true,
	"ogg":  true,
	"opus": true,
	"flac": true,
	"pcm":  true,
}

func (s *Server) validateSpeech(req provider.SpeechRequest) error {
	if strings.TrimSpace(req.Input) == "" {
		return errors.New("input is required")
	}
	if utf8.RuneCountInString(req.Input) > s.maxInputChars {
		return fmt.Errorf("input is too long; max %d characters", s.maxInputChars)
	}
	format := normalizedFormat(req.ResponseFormat)
	if !supportedFormats[format] {
		return errors.New("response_format must be one of: mp3, wav, ogg, opus, flac, pcm")
	}
	if req.Speed != 0 && (req.Speed < 0.25 || req.Speed > 4.0) {
		return errors.New("speed must be between 0.25 and 4.0")
	}
	return nil
}

// resolveVoice gives the API a provider-independent voice contract. An empty
// voice is normalized to auto; an explicit voice must be present in the
// provider's language-filtered catalog. Providers still own the actual auto
// default (for example Supertonic's M1 or eSpeak's language voice).
func (s *Server) resolveVoice(ctx context.Context, p provider.Provider, req *provider.SpeechRequest) error {
	req.Language = requestLanguage(req.Language)
	if req.Language == "auto" && !supportsAutoLanguage(p) {
		return fmt.Errorf("%w: provider %q does not support language auto", provider.ErrInvalidRequest, p.ID())
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = "auto"
	}
	req.Voice = voice

	voices, err := p.Voices(ctx, req.Language)
	if err != nil {
		return err
	}
	if len(voices) == 0 {
		if req.Language != "" {
			return fmt.Errorf("%w: language %q is not supported by provider %q", provider.ErrInvalidRequest, req.Language, p.ID())
		}
		return fmt.Errorf("%w: provider %q has no available voices", provider.ErrInvalidRequest, p.ID())
	}
	if strings.EqualFold(voice, "auto") {
		req.Voice = "auto"
		return nil
	}
	for _, candidate := range voices {
		if strings.EqualFold(strings.TrimSpace(candidate.Voice), voice) {
			// Preserve the provider's canonical spelling (Supertonic uses M1/F1).
			req.Voice = candidate.Voice
			return nil
		}
	}
	return fmt.Errorf("%w: voice %q is not available for provider %q and language %q", provider.ErrInvalidRequest, voice, p.ID(), req.Language)
}

func supportsAutoLanguage(p provider.Provider) bool {
	autoProvider, ok := p.(provider.AutoLanguageProvider)
	return ok && autoProvider.SupportsAutoLanguage()
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

func requestLanguage(language string) string {
	if language = normalizeLanguage(language); language == "" {
		return "auto"
	}
	return language
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

// ── alignment endpoints (active when AlignProvider is configured) ──

func (s *Server) createAlignJob(w http.ResponseWriter, r *http.Request) {
	if s.alignProvider == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("alignment not configured"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxUploadBytes))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("audio file is required"))
		return
	}
	defer file.Close()
	audio, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	transcript := r.FormValue("transcript")
	if strings.TrimSpace(transcript) == "" {
		writeError(w, http.StatusBadRequest, errors.New("transcript is required"))
		return
	}
	language := r.FormValue("language")
	if !s.alignProvider.HasLanguage(language) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported language: %s", language))
		return
	}

	job := s.queue.NewRunningJob("align", provider.SpeechRequest{Input: transcript})
	go func() {
		result, err := s.alignProvider.Align(context.Background(), audio, transcript, language)
		if err != nil {
			s.queue.FailJob(job.ID, err)
			return
		}
		s.queue.CompleteJob(job.ID, provider.SpeechResult{
			Model:      "align/" + language,
			ProviderID: "align",
			Voice:      language,
		})
		if result != nil {
			s.queue.SetAlignResult(job.ID, result)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.jobStatusBody(job.ID))
}

func (s *Server) getAlignJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.queue.Get(id); !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown alignment job id"))
		return
	}
	body := s.jobStatusBody(id)
	ar := s.queue.AlignResult(id)
	if ar != nil {
		body["result"] = ar
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) getAlignResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.queue.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("unknown alignment job id"))
		return
	}
	switch job.Status {
	case queue.StatusSucceeded:
		ar := s.queue.AlignResult(id)
		if ar == nil {
			writeError(w, http.StatusNotFound, errors.New("alignment result not available"))
			return
		}
		writeJSON(w, http.StatusOK, ar)
	case queue.StatusFailed:
		writeError(w, http.StatusNotFound, fmt.Errorf("alignment failed: %v", job.Err))
	default:
		writeError(w, http.StatusConflict, errors.New("alignment is not finished yet"))
	}
}

func (s *Server) listAlignLanguages(w http.ResponseWriter, r *http.Request) {
	languages := s.alignProvider.Languages()
	sort.Strings(languages)
	writeJSON(w, http.StatusOK, map[string]any{"languages": languages})
}
