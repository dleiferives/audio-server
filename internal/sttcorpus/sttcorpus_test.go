package sttcorpus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dleiferives/audio-server/internal/pcmwav"
)

// tone builds ms milliseconds of 16 kHz mono PCM16 whose samples encode their
// own index, so a clip can be checked for coming from the right offset.
func tone(ms int) []byte {
	samples := 16000 * ms / 1000
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		pcm[2*i] = byte(i & 0xff)
		pcm[2*i+1] = byte((i >> 8) & 0x7f)
	}
	return pcm
}

func readManifest(t *testing.T, dir string) []Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestRecordUploadWritesWAVAndManifest(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := encodePCM(tone(500), 16000)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := r.RecordUpload(wav, "hello world", Meta{Provider: "moonshine", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Source != SourceUpload || entry.Text != "hello world" {
		t.Fatalf("unexpected entry %+v", entry)
	}
	if entry.DurationS < 0.49 || entry.DurationS > 0.51 {
		t.Fatalf("unexpected duration %v", entry.DurationS)
	}
	if _, err := os.Stat(filepath.Join(dir, entry.Audio)); err != nil {
		t.Fatalf("audio not written: %v", err)
	}

	entries := readManifest(t, dir)
	if len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("unexpected manifest %+v", entries)
	}
	if entries[0].Provider != "moonshine" || entries[0].Language != "en" {
		t.Fatalf("metadata not recorded: %+v", entries[0])
	}
}

func TestRecordUploadRejectsNonWAV(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordUpload([]byte("not audio"), "text", Meta{}); err == nil {
		t.Fatal("expected an error for non-WAV input")
	}
}

func TestSessionWritesSessionAndLineClips(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	session := r.NewSession(16000, Meta{Provider: "moonshine", Language: "en"})
	// Feed 3 seconds in 500ms frames, the way the live handler does.
	full := tone(3000)
	for i := 0; i < len(full); i += 16000 {
		end := i + 16000
		if end > len(full) {
			end = len(full)
		}
		session.Append(full[i:end])
	}
	if session.Bytes() != len(full) {
		t.Fatalf("buffered %d bytes, want %d", session.Bytes(), len(full))
	}

	entries, err := session.Finish("first line second line", []Line{
		{Text: "first line", StartMS: 0, DurationMS: 1000},
		{Text: "second line", StartMS: 1000, DurationMS: 2000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected a session entry plus 2 line entries, got %d", len(entries))
	}

	if entries[0].Source != SourceSession || entries[0].SessionID != entries[0].ID {
		t.Fatalf("unexpected session entry %+v", entries[0])
	}
	if entries[0].DurationS < 2.99 || entries[0].DurationS > 3.01 {
		t.Fatalf("unexpected session duration %v", entries[0].DurationS)
	}

	// The second line must start one second in and last two seconds, and its
	// audio must be the matching slice of the session.
	line := entries[2]
	if line.Source != SourceLine || line.OffsetS != 1 || line.DurationS != 2 {
		t.Fatalf("unexpected line entry %+v", line)
	}
	if line.LineIndex == nil || *line.LineIndex != 1 {
		t.Fatalf("expected line_index 1, got %+v", line.LineIndex)
	}
	if line.SessionAudio != entries[0].Audio {
		t.Fatalf("line should reference the session audio, got %q", line.SessionAudio)
	}

	clip, err := os.ReadFile(filepath.Join(dir, line.Audio))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pcmwav.Parse(clip)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Samples) != 32000 {
		t.Fatalf("clip has %d samples, want 32000", len(parsed.Samples))
	}
	// Sample values encode their index in the session, so the first sample of
	// this clip proves the offset was applied.
	if got := parsed.Samples[0]; got != int16(16000&0xff)|int16((16000>>8)&0x7f)<<8 {
		t.Fatalf("clip starts at the wrong offset, first sample %d", got)
	}

	if len(readManifest(t, dir)) != 3 {
		t.Fatal("manifest should hold all three rows")
	}
}

func TestSessionSkipsLinesOutsideTheAudio(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := r.NewSession(16000, Meta{Provider: "moonshine"})
	session.Append(tone(1000))

	// A line running past the buffered audio would otherwise be written as a
	// truncated clip that disagrees with its text.
	entries, err := session.Finish("only line", []Line{
		{Text: "past the end", StartMS: 500, DurationMS: 5000},
		{Text: "zero length", StartMS: 0, DurationMS: 0},
		{Text: "", StartMS: 0, DurationMS: 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the session entry, got %d entries", len(entries))
	}
}

func TestEmptySessionWritesNothing(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.NewSession(16000, Meta{}).Finish("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		t.Fatalf("expected no entries, got %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); !os.IsNotExist(err) {
		t.Fatal("an empty session must not create a manifest")
	}
}

func TestAppendsAccumulateAcrossRecordings(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := encodePCM(tone(200), 16000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.RecordUpload(wav, "line", Meta{Provider: "moonshine"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(readManifest(t, dir)); got != 3 {
		t.Fatalf("manifest has %d rows, want 3", got)
	}
}
