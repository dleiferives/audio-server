// Package sttcorpus persists transcribed audio as an ASR fine-tuning corpus.
//
// This is deliberately separate from internal/store. That store holds generated
// TTS output and separation stems and is swept on a TTL; a corpus recorded for
// training must never be swept, so it lives in its own directory with no
// expiry. Capture is opt-in: nothing is written unless a recorder is configured.
//
// The layout is the shape ASR fine-tuning pipelines expect — 16 kHz mono WAV
// files plus an append-only JSON Lines manifest that pairs each clip with its
// text and metadata:
//
//	<dir>/manifest.jsonl
//	<dir>/uploads/2026-09-19/<id>.wav
//	<dir>/sessions/2026-09-19/<id>.wav
//	<dir>/lines/2026-09-19/<id>-000.wav
//
// Live sessions record both the whole session and one clip per completed line,
// so the corpus can be used either way without re-segmenting. Line clips carry
// offset_s and session_audio so they can be traced back to the session they
// came from.
package sttcorpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dleiferives/audio-server/internal/pcmwav"
)

const (
	manifestName = "manifest.jsonl"

	// SourceUpload is a buffered POST /v1/audio/transcriptions upload.
	SourceUpload = "upload"
	// SourceSession is a whole live WebSocket session.
	SourceSession = "live-session"
	// SourceLine is one completed line carved out of a live session.
	SourceLine = "live-line"
)

// Entry is one row of the manifest. Field names are snake_case so the manifest
// loads directly as a Hugging Face / Whisper-style dataset.
type Entry struct {
	ID           string  `json:"id"`
	Audio        string  `json:"audio"`
	Text         string  `json:"text"`
	DurationS    float64 `json:"duration_s"`
	SampleRate   int     `json:"sample_rate"`
	Channels     int     `json:"channels"`
	Language     string  `json:"language,omitempty"`
	Provider     string  `json:"provider"`
	Model        string  `json:"model,omitempty"`
	Source       string  `json:"source"`
	CreatedAt    string  `json:"created_at"`
	SessionID    string  `json:"session_id,omitempty"`
	LineIndex    *int    `json:"line_index,omitempty"`
	OffsetS      float64 `json:"offset_s,omitempty"`
	SessionAudio string  `json:"session_audio,omitempty"`
}

// Line is one completed transcript line within a live session.
type Line struct {
	Text       string
	StartMS    int64
	DurationMS int64
}

// Meta describes the request a recording came from.
type Meta struct {
	Provider string
	Model    string
	Language string
}

type Recorder struct {
	dir string

	// mu serializes manifest appends so concurrent requests cannot interleave
	// partial lines.
	mu sync.Mutex
}

func New(dir string) (*Recorder, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("sttcorpus: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("sttcorpus: %w", err)
	}
	return &Recorder{dir: abs}, nil
}

func (r *Recorder) Dir() string { return r.dir }

// RecordUpload stores a buffered upload and its transcript. The WAV must already
// be the canonical mono PCM16 the STT providers are given.
func (r *Recorder) RecordUpload(wav []byte, text string, meta Meta) (Entry, error) {
	audio, err := pcmwav.Parse(wav)
	if err != nil {
		return Entry{}, fmt.Errorf("sttcorpus: parse upload: %w", err)
	}
	now := time.Now().UTC()
	id := newID(now)
	rel := filepath.Join(SourceUpload+"s", now.Format("2006-01-02"), id+".wav")
	if err := r.writeFile(rel, wav); err != nil {
		return Entry{}, err
	}
	entry := Entry{
		ID:         id,
		Audio:      filepath.ToSlash(rel),
		Text:       text,
		DurationS:  duration(len(audio.Samples), audio.SampleRate),
		SampleRate: audio.SampleRate,
		Channels:   1,
		Language:   meta.Language,
		Provider:   meta.Provider,
		Model:      meta.Model,
		Source:     SourceUpload,
		CreatedAt:  now.Format(time.RFC3339),
	}
	if err := r.append(entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Session accumulates the PCM fed through a live stream so it can be written
// once the session finishes. It is not safe for concurrent use; the live
// handler drives one session from a single goroutine.
type Session struct {
	recorder   *Recorder
	meta       Meta
	sampleRate int
	pcm        []byte
	started    time.Time
}

// NewSession starts recording a live session. sampleRate is the rate of the PCM
// that will be appended.
func (r *Recorder) NewSession(sampleRate int, meta Meta) *Session {
	return &Session{recorder: r, meta: meta, sampleRate: sampleRate, started: time.Now().UTC()}
}

// Append buffers a PCM16 frame exactly as it was fed to the provider.
func (s *Session) Append(pcm []byte) {
	if s == nil || len(pcm) == 0 {
		return
	}
	s.pcm = append(s.pcm, pcm...)
}

// Bytes reports how much audio has been buffered, so a caller can enforce its
// own ceiling before memory grows without bound.
func (s *Session) Bytes() int {
	if s == nil {
		return 0
	}
	return len(s.pcm)
}

// Finish writes the session WAV plus one clip per completed line. Lines whose
// window falls outside the buffered audio are skipped rather than truncated, so
// a clip never silently disagrees with its text. Returns the entries written.
func (s *Session) Finish(text string, lines []Line) ([]Entry, error) {
	if s == nil || len(s.pcm) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	day := s.started.Format("2006-01-02")
	id := newID(s.started)

	sessionRel := filepath.Join("sessions", day, id+".wav")
	wav, err := encodePCM(s.pcm, s.sampleRate)
	if err != nil {
		return nil, err
	}
	if err := s.recorder.writeFile(sessionRel, wav); err != nil {
		return nil, err
	}

	entries := []Entry{{
		ID:         id,
		Audio:      filepath.ToSlash(sessionRel),
		Text:       text,
		DurationS:  duration(len(s.pcm)/2, s.sampleRate),
		SampleRate: s.sampleRate,
		Channels:   1,
		Language:   s.meta.Language,
		Provider:   s.meta.Provider,
		Model:      s.meta.Model,
		Source:     SourceSession,
		CreatedAt:  now.Format(time.RFC3339),
		SessionID:  id,
	}}

	for i, line := range lines {
		if line.Text == "" || line.DurationMS <= 0 {
			continue
		}
		start := byteOffset(line.StartMS, s.sampleRate)
		end := byteOffset(line.StartMS+line.DurationMS, s.sampleRate)
		if start < 0 || end > len(s.pcm) || end <= start {
			continue
		}
		index := i
		lineID := fmt.Sprintf("%s-%03d", id, i)
		lineRel := filepath.Join("lines", day, lineID+".wav")
		lineWAV, err := encodePCM(s.pcm[start:end], s.sampleRate)
		if err != nil {
			return nil, err
		}
		if err := s.recorder.writeFile(lineRel, lineWAV); err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			ID:           lineID,
			Audio:        filepath.ToSlash(lineRel),
			Text:         line.Text,
			DurationS:    float64(line.DurationMS) / 1000,
			SampleRate:   s.sampleRate,
			Channels:     1,
			Language:     s.meta.Language,
			Provider:     s.meta.Provider,
			Model:        s.meta.Model,
			Source:       SourceLine,
			CreatedAt:    now.Format(time.RFC3339),
			SessionID:    id,
			LineIndex:    &index,
			OffsetS:      float64(line.StartMS) / 1000,
			SessionAudio: filepath.ToSlash(sessionRel),
		})
	}

	if err := s.recorder.append(entries...); err != nil {
		return nil, err
	}
	return entries, nil
}

func (r *Recorder) writeFile(rel string, data []byte) error {
	path := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sttcorpus: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("sttcorpus: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("sttcorpus: %w", err)
	}
	return nil
}

// append adds rows to the manifest. Rows are written in one call so the entries
// for a single session cannot be split by a concurrent request.
func (r *Recorder) append(entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	var buf []byte
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("sttcorpus: %w", err)
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := os.OpenFile(filepath.Join(r.dir, manifestName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("sttcorpus: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(buf); err != nil {
		return fmt.Errorf("sttcorpus: %w", err)
	}
	return nil
}

func encodePCM(pcm []byte, sampleRate int) ([]byte, error) {
	audio := pcmwav.Audio{SampleRate: sampleRate, Samples: make([]int16, len(pcm)/2)}
	for i := range audio.Samples {
		audio.Samples[i] = int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
	}
	wav, err := pcmwav.Encode(audio)
	if err != nil {
		return nil, fmt.Errorf("sttcorpus: encode wav: %w", err)
	}
	return wav, nil
}

// byteOffset converts a millisecond position to a sample-aligned byte offset.
func byteOffset(ms int64, sampleRate int) int {
	offset := int(ms) * sampleRate / 1000 * 2
	if offset < 0 {
		return -1
	}
	return offset
}

func duration(samples, sampleRate int) float64 {
	if sampleRate <= 0 {
		return 0
	}
	return float64(samples) / float64(sampleRate)
}

func newID(now time.Time) string {
	return fmt.Sprintf("%s-%d", now.Format("20060102T150405"), now.UnixNano()%1e9)
}
