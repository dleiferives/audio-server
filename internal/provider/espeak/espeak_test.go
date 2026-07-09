package espeak

import (
	"context"
	"errors"
	"testing"

	"github.com/dleiferives/audio-server/internal/provider"
)

func TestWPM(t *testing.T) {
	tests := []struct {
		speed float64
		want  int
	}{
		{0, 175},
		{1, 175},
		{0.5, 88},
		{0.01, 80},
		{4, 450},
	}
	for _, tt := range tests {
		if got := WPM(tt.speed); got != tt.want {
			t.Fatalf("WPM(%v) = %d, want %d", tt.speed, got, tt.want)
		}
	}
}

func TestSynthesizeBuildsEspeakCommandAndEncodes(t *testing.T) {
	var gotName string
	var gotArgs []string
	var gotInput string
	p := New("espeak-test", "en", fakeEncoder{audio: []byte("mp3"), contentType: "audio/mpeg"})
	p.Run = func(_ context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		gotInput = string(stdin)
		return []byte("wav"), nil, nil
	}

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:          "hello",
		Voice:          "en-us",
		ResponseFormat: "mp3",
		Speed:          1.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "mp3" || result.ContentType != "audio/mpeg" || result.ProviderID != "espeak-ng" || result.Voice != "en-us" {
		t.Fatalf("unexpected result: %+v", result)
	}
	wantArgs := []string{"--stdout", "--stdin", "-b", "1", "-v", "en-us", "-s", "263"}
	if gotName != "espeak-test" || gotInput != "hello" || !equal(gotArgs, wantArgs) {
		t.Fatalf("unexpected command: name=%q args=%v input=%q", gotName, gotArgs, gotInput)
	}
}

func TestSynthesizeWAVSkipsEncoder(t *testing.T) {
	p := New("espeak-test", "en", fakeEncoder{err: errors.New("should not run")})
	p.Run = func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return []byte("wav"), nil, nil
	}

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:          "hello",
		Language:       "EN_US",
		ResponseFormat: "wav",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "wav" || result.ContentType != "audio/wav" || result.Voice != "en-us" {
		t.Fatalf("unexpected wav result: %+v", result)
	}
}

func TestVoicesParsesEspeakOutput(t *testing.T) {
	p := New("espeak-test", "en", nil)
	p.Run = func(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
		wantArgs := []string{"--voices=el"}
		if !equal(args, wantArgs) {
			t.Fatalf("args = %v, want %v", args, wantArgs)
		}
		return []byte(`Pty Language Age/Gender VoiceName File Other Languages
 5  el              --/M      Greek          europe/el
 5  en-us           --/F      English_(America) english-us
`), nil, nil
	}

	voices, err := p.Voices(context.Background(), "EL")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 2 {
		t.Fatalf("voices len = %d", len(voices))
	}
	if voices[0].Provider != "espeak-ng" || voices[0].Language != "el" || voices[0].Voice != "Greek" || voices[0].Gender != "M" {
		t.Fatalf("unexpected first voice: %+v", voices[0])
	}
}

func TestSynthesizeCommandFailureIsUnavailable(t *testing.T) {
	p := New("espeak-test", "en", nil)
	p.Run = func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		return nil, []byte("nope"), errors.New("exit 1")
	}
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hello", ResponseFormat: "wav"})
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

type fakeEncoder struct {
	audio       []byte
	contentType string
	err         error
}

func (f fakeEncoder) Health(context.Context) error {
	return f.err
}

func (f fakeEncoder) Encode(context.Context, []byte, string) ([]byte, string, error) {
	return f.audio, f.contentType, f.err
}

func equal(a, b []string) bool {
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
