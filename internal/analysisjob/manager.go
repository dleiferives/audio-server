// Package analysisjob runs asynchronous diarization and speaker-embedding jobs.
package analysisjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
	"github.com/dleiferives/audio-server/internal/pcmwav"
	"github.com/dleiferives/audio-server/internal/sttprovider"
)

var ErrNotFound = errors.New("analysis job not found")

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Request struct {
	Audio                    []byte
	Filename                 string
	IncludeSpeakerEmbeddings bool
	Transcribe               bool
	TranscriptionModel       string
	DiarizationModel         string
	Language                 string
	SeparateDialogue         bool
	SeparationModel          string

	// NumSpeakers/MinSpeakers/MaxSpeakers are optional cast-size hints for
	// diarizers that support constraining cluster count (e.g. pyannote).
	NumSpeakers int
	MinSpeakers int
	MaxSpeakers int
}

type Segment struct {
	StartMS    int64    `json:"start_ms"`
	EndMS      int64    `json:"end_ms"`
	SpeakerIDs []string `json:"speaker_ids"`
	Confidence float64  `json:"confidence"`
	Overlap    bool     `json:"overlap"`
	Text       string   `json:"text,omitempty"`
}

type Speaker struct {
	ID             string    `json:"id"`
	SpeechMS       int64     `json:"speech_ms"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
}

type Result struct {
	DurationMS  int64                          `json:"duration_ms"`
	SampleRate  int                            `json:"sample_rate"`
	Diarizer    string                         `json:"diarization_model"`
	Embedder    string                         `json:"speaker_embedding_model,omitempty"`
	Transcriber string                         `json:"transcription_model,omitempty"`
	Separator   string                         `json:"dialogue_separation_model,omitempty"`
	Speakers    []Speaker                      `json:"speakers"`
	Segments    []Segment                      `json:"segments"`
	RawTurns    []analysisprovider.SpeakerTurn `json:"raw_turns"`
}

type Job struct {
	ID         string
	Status     Status
	Result     Result
	Err        error
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

type Config struct {
	Diarizers          map[string]analysisprovider.Diarizer
	DefaultDiarizer    string
	Embedder           analysisprovider.Embedder
	Separators         map[string]analysisprovider.Separator
	DefaultSeparator   string
	AudioNormalizer    sttprovider.AudioNormalizer
	Transcribers       map[string]sttprovider.Provider
	DefaultTranscriber string
	RunGate            func(providerID string) (func(), error)
	Timeout            time.Duration
}

type Manager struct {
	mu                 sync.RWMutex
	jobs               map[string]*Job
	diarizers          map[string]analysisprovider.Diarizer
	defaultDiarizer    string
	embedder           analysisprovider.Embedder
	separators         map[string]analysisprovider.Separator
	defaultSeparator   string
	audioNormalizer    sttprovider.AudioNormalizer
	transcribers       map[string]sttprovider.Provider
	defaultTranscriber string
	runGate            func(string) (func(), error)
	timeout            time.Duration
}

func New(cfg Config) *Manager {
	return &Manager{
		jobs: make(map[string]*Job), diarizers: cfg.Diarizers, defaultDiarizer: cfg.DefaultDiarizer,
		embedder: cfg.Embedder, separators: cfg.Separators, defaultSeparator: cfg.DefaultSeparator,
		audioNormalizer: cfg.AudioNormalizer,
		transcribers:    cfg.Transcribers, defaultTranscriber: cfg.DefaultTranscriber,
		runGate:         cfg.RunGate, timeout: cfg.Timeout,
	}
}

func (m *Manager) Enabled() bool { return m != nil && len(m.diarizers) > 0 }

// DiarizerID reports the default diarizer's id, for callers (e.g. discovery
// endpoints) that don't care about per-job selection.
func (m *Manager) DiarizerID() string {
	if !m.Enabled() {
		return ""
	}
	if d, ok := m.diarizers[m.defaultDiarizer]; ok {
		return d.ID()
	}
	return ""
}

// DiarizerIDs reports every selectable diarization model id.
func (m *Manager) DiarizerIDs() []string {
	ids := make([]string, 0, len(m.diarizers))
	for id := range m.diarizers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) EmbedderID() string {
	if m == nil || m.embedder == nil {
		return ""
	}
	return m.embedder.ID()
}

// SeparatorID reports the default separator's id, for callers (e.g.
// discovery endpoints) that don't care about per-job selection.
func (m *Manager) SeparatorID() string {
	if m == nil {
		return ""
	}
	if s, ok := m.separators[m.defaultSeparator]; ok {
		return s.ID()
	}
	return ""
}

// SeparatorIDs reports every selectable dialogue-separation model id.
func (m *Manager) SeparatorIDs() []string {
	if m == nil {
		return nil
	}
	ids := make([]string, 0, len(m.separators))
	for id := range m.separators {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) Submit(req Request) (*Job, error) {
	if !m.Enabled() {
		return nil, errors.New("audio analysis is not configured")
	}
	if len(req.Audio) == 0 {
		return nil, errors.New("audio is required")
	}
	if req.Transcribe {
		model := req.TranscriptionModel
		if model == "" || model == "auto" || model == "whisper-1" {
			model = m.defaultTranscriber
		}
		if _, ok := m.transcribers[model]; !ok {
			return nil, fmt.Errorf("unknown analysis transcription model %q", req.TranscriptionModel)
		}
		req.TranscriptionModel = model
	}
	diarModel := req.DiarizationModel
	if diarModel == "" || diarModel == "auto" {
		diarModel = m.defaultDiarizer
	}
	if _, ok := m.diarizers[diarModel]; !ok {
		return nil, fmt.Errorf("unknown analysis diarization model %q", req.DiarizationModel)
	}
	req.DiarizationModel = diarModel
	if req.SeparateDialogue {
		sepModel := req.SeparationModel
		if sepModel == "" || sepModel == "auto" {
			sepModel = m.defaultSeparator
		}
		if _, ok := m.separators[sepModel]; !ok {
			return nil, fmt.Errorf("unknown analysis separation model %q", req.SeparationModel)
		}
		req.SeparationModel = sepModel
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job := &Job{ID: id, Status: StatusQueued, CreatedAt: time.Now()}
	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()
	go m.run(job, req)
	return clone(job), nil
}

func (m *Manager) Get(id string) (*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(job), nil
}

func (m *Manager) run(job *Job, req Request) {
	m.mu.Lock()
	job.Status = StatusRunning
	job.StartedAt = time.Now()
	m.mu.Unlock()

	ctx := context.Background()
	var cancel context.CancelFunc
	if m.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
	result, err := m.analyze(ctx, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	job.FinishedAt = time.Now()
	job.Result = result
	job.Err = err
	if err != nil {
		job.Status = StatusFailed
	} else {
		job.Status = StatusSucceeded
	}
}

func (m *Manager) analyze(ctx context.Context, req Request) (Result, error) {
	if m.audioNormalizer == nil {
		return Result{}, errors.New("audio normalization is not configured")
	}
	separatorID := ""
	if req.SeparateDialogue {
		separator, ok := m.separators[req.SeparationModel]
		if !ok {
			return Result{}, fmt.Errorf("unknown analysis separation model %q", req.SeparationModel)
		}
		separationInput, err := m.audioNormalizer.NormalizeAudio(ctx, req.Audio, sttprovider.AudioFormat{
			Container: "wav", Codec: "pcm_s16le", SampleRate: 44100, Channels: 2,
		})
		if err != nil {
			return Result{}, fmt.Errorf("normalize dialogue separation input: %w", err)
		}
		var release func()
		if m.runGate != nil {
			release, err = m.runGate(separator.ID())
			if err != nil {
				return Result{}, fmt.Errorf("acquire dialogue separation model: %w", err)
			}
		}
		if release != nil {
			defer func() {
				if release != nil {
					release()
				}
			}()
		}
		separated, err := separator.Separate(ctx, analysisprovider.AudioRequest{Audio: separationInput.Audio, Filename: req.Filename})
		if err != nil {
			return Result{}, err
		}
		if release != nil {
			release()
			release = nil
		}
		var vocals []byte
		for id, audio := range separated.Outputs {
			if strings.Contains(strings.ToLower(id), "vocal") || strings.Contains(strings.ToLower(id), "dialog") {
				vocals = audio
				break
			}
		}
		if len(vocals) == 0 {
			return Result{}, errors.New("dialogue separation returned no vocals stem")
		}
		normalized, err := m.audioNormalizer.NormalizeAudio(ctx, vocals, sttprovider.AudioFormat{Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1})
		if err != nil {
			return Result{}, fmt.Errorf("normalize separated dialogue: %w", err)
		}
		req.Audio = normalized.Audio
		separatorID = separated.ProviderID
	} else {
		normalized, err := m.audioNormalizer.NormalizeAudio(ctx, req.Audio, sttprovider.AudioFormat{
			Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1,
		})
		if err != nil {
			return Result{}, fmt.Errorf("normalize analysis input: %w", err)
		}
		req.Audio = normalized.Audio
	}
	audio, err := pcmwav.Parse(req.Audio)
	if err != nil {
		return Result{}, fmt.Errorf("parse normalized audio: %w", err)
	}
	diarizer, ok := m.diarizers[req.DiarizationModel]
	if !ok {
		return Result{}, fmt.Errorf("unknown analysis diarization model %q", req.DiarizationModel)
	}
	var release func()
	if m.runGate != nil {
		release, err = m.runGate(diarizer.ID())
		if err != nil {
			return Result{}, fmt.Errorf("acquire diarization model: %w", err)
		}
	}
	if release != nil {
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	diar, err := diarizer.Diarize(ctx, analysisprovider.AudioRequest{
		Audio: req.Audio, Filename: req.Filename,
		NumSpeakers: req.NumSpeakers, MinSpeakers: req.MinSpeakers, MaxSpeakers: req.MaxSpeakers,
	})
	if err != nil {
		return Result{}, err
	}
	if release != nil {
		release()
		release = nil
	}
	if diar.SampleRate <= 0 {
		diar.SampleRate = audio.SampleRate
	}
	result := Result{
		DurationMS: int64(len(audio.Samples)) * 1000 / int64(audio.SampleRate),
		SampleRate: diar.SampleRate,
		Diarizer:   diar.ProviderID,
		Segments:   mergeTurns(diar.Turns, diar.SampleRate),
		RawTurns:   append([]analysisprovider.SpeakerTurn(nil), diar.Turns...),
		Separator:  separatorID,
	}
	if req.Transcribe {
		transcriber := m.transcribers[req.TranscriptionModel]
		if err := m.transcribeSegments(ctx, transcriber, audio, result.Segments, req.Language); err != nil {
			return Result{}, err
		}
		result.Transcriber = transcriber.ID()
	}
	speakerSamples := collectSpeakerSamples(audio.Samples, diar.Turns, 30*audio.SampleRate)
	ids := make([]string, 0, len(speakerSamples))
	for id := range speakerSamples {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		samples := speakerSamples[id]
		speaker := Speaker{ID: id, SpeechMS: int64(len(samples)) * 1000 / int64(audio.SampleRate)}
		switch {
		case !req.IncludeSpeakerEmbeddings:
			// no embedding requested
		case len(diar.SpeakerEmbeddings) > 0:
			// The diarizer already computed embeddings as part of its
			// whole-clip pass (e.g. pyannote); reuse them instead of running
			// a separate embedding provider over reconstructed samples.
			if emb, ok := diar.SpeakerEmbeddings[id]; ok {
				speaker.EmbeddingModel = diar.EmbeddingModel
				speaker.Embedding = emb
				result.Embedder = diar.ProviderID
			}
		case m.embedder != nil && len(samples) >= audio.SampleRate/4:
			wav, encodeErr := pcmwav.Encode(pcmwav.Audio{SampleRate: audio.SampleRate, Samples: samples})
			if encodeErr != nil {
				return Result{}, encodeErr
			}
			emb, embedErr := m.embedder.Embed(ctx, analysisprovider.AudioRequest{Audio: wav, Filename: id + ".wav"})
			if embedErr != nil {
				return Result{}, embedErr
			}
			speaker.EmbeddingModel = emb.Model
			speaker.Embedding = emb.Embedding
			result.Embedder = emb.ProviderID
		}
		result.Speakers = append(result.Speakers, speaker)
	}
	return result, nil
}

func (m *Manager) transcribeSegments(ctx context.Context, provider sttprovider.Provider, audio pcmwav.Audio, segments []Segment, language string) error {
	var release func()
	var err error
	if m.runGate != nil {
		release, err = m.runGate(provider.ID())
		if err != nil {
			return fmt.Errorf("acquire transcription model: %w", err)
		}
	}
	if release != nil {
		defer release()
	}
	for i := range segments {
		start := max(0, min(len(audio.Samples), int(segments[i].StartMS)*audio.SampleRate/1000))
		end := max(start, min(len(audio.Samples), int(segments[i].EndMS)*audio.SampleRate/1000))
		if end-start < audio.SampleRate/4 {
			continue
		}
		wav, err := pcmwav.Encode(pcmwav.Audio{SampleRate: audio.SampleRate, Samples: audio.Samples[start:end]})
		if err != nil {
			return err
		}
		transcript, err := provider.Transcribe(ctx, sttprovider.TranscriptionRequest{
			Audio: wav, Filename: fmt.Sprintf("segment-%06d.wav", i), Language: language, Model: provider.ID(),
		})
		if err != nil {
			return fmt.Errorf("transcribe segment %d: %w", i, err)
		}
		segments[i].Text = transcript.Text
	}
	return nil
}

func collectSpeakerSamples(samples []int16, turns []analysisprovider.SpeakerTurn, limit int) map[string][]int16 {
	result := make(map[string][]int16)
	for _, turn := range turns {
		start := max(0, min(len(samples), int(turn.StartSample)))
		end := max(start, min(len(samples), int(turn.EndSample)))
		remaining := limit - len(result[turn.SpeakerID])
		if remaining <= 0 || start == end {
			continue
		}
		if end-start > remaining {
			end = start + remaining
		}
		result[turn.SpeakerID] = append(result[turn.SpeakerID], samples[start:end]...)
	}
	return result
}

func mergeTurns(turns []analysisprovider.SpeakerTurn, sampleRate int) []Segment {
	if sampleRate <= 0 || len(turns) == 0 {
		return []Segment{}
	}
	boundaries := make([]int64, 0, len(turns)*2)
	for _, turn := range turns {
		if turn.EndSample > turn.StartSample {
			boundaries = append(boundaries, turn.StartSample, turn.EndSample)
		}
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i] < boundaries[j] })
	unique := boundaries[:0]
	for _, value := range boundaries {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	segments := make([]Segment, 0, len(unique))
	for i := 0; i+1 < len(unique); i++ {
		start, end := unique[i], unique[i+1]
		var speakers []string
		confidence := 0.0
		for _, turn := range turns {
			if turn.StartSample < end && turn.EndSample > start {
				speakers = append(speakers, turn.SpeakerID)
				confidence += turn.Confidence
			}
		}
		if len(speakers) == 0 {
			continue
		}
		sort.Strings(speakers)
		speakers = uniqueStrings(speakers)
		segment := Segment{
			StartMS: start * 1000 / int64(sampleRate), EndMS: end * 1000 / int64(sampleRate),
			SpeakerIDs: speakers, Confidence: confidence / float64(len(speakers)), Overlap: len(speakers) > 1,
		}
		if len(segments) > 0 && segments[len(segments)-1].EndMS == segment.StartMS && sameStrings(segments[len(segments)-1].SpeakerIDs, segment.SpeakerIDs) {
			segments[len(segments)-1].EndMS = segment.EndMS
			segments[len(segments)-1].Confidence = (segments[len(segments)-1].Confidence + segment.Confidence) / 2
		} else {
			segments = append(segments, segment)
		}
	}
	return segments
}

func uniqueStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func clone(job *Job) *Job {
	copy := *job
	return &copy
}

func newID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create analysis job id: %w", err)
	}
	return "analysis_" + hex.EncodeToString(value[:]), nil
}
