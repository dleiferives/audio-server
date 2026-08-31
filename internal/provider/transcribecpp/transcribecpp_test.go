package transcribecpp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestTranscribe(t *testing.T) {
	started := false
	provider := New("cohere-transcribe", "http://127.0.0.1:8031", roundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/transcribe" || req.Header.Get("X-Transcribe-Language") != "en" {
			t.Fatalf("unexpected request: %s language=%q", req.URL.Path, req.Header.Get("X-Transcribe-Language"))
		}
		body, _ := io.ReadAll(req.Body)
		if string(body) != "wav" {
			t.Fatalf("body = %q", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":"hello","language":"en","duration":1.25}`))}, nil
	}))
	provider.StartFunc = func() error { started = true; return nil }

	result, err := provider.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio: []byte("wav"), Language: "en-US",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !started || result.Text != "hello" || result.Language != "en" || result.Duration != 1.25 || result.ProviderID != "cohere-transcribe" {
		t.Fatalf("unexpected result: started=%v result=%+v", started, result)
	}
}

func TestAudioRequirements(t *testing.T) {
	got := New("cohere-transcribe", "http://localhost", nil).AudioRequirements()
	want := (sttprovider.AudioFormat{Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1})
	if got != want {
		t.Fatalf("requirements = %+v, want %+v", got, want)
	}
}
