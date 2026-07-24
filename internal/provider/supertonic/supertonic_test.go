package supertonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/provider"
)

type fakeClient struct {
	do func(*http.Request) (*http.Response, error)
}

func (f fakeClient) Do(req *http.Request) (*http.Response, error) {
	return f.do(req)
}

func TestSynthesizePCMUsesBufferedAudioStream(t *testing.T) {
	var body speechRequest
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/audio/speech" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		if req.Header.Get("Accept") != "audio/pcm" {
			t.Fatalf("Accept = %q, want audio/pcm", req.Header.Get("Accept"))
		}
		encoded, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("raw-pcm")),
		}, nil
	}}, nil)

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:          "hello",
		Language:       "auto",
		Voice:          "auto",
		ResponseFormat: "pcm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.ResponseFormat != "pcm" || body.StreamFormat != "audio" {
		t.Fatalf("sidecar request formats = response:%q stream:%q", body.ResponseFormat, body.StreamFormat)
	}
	if string(result.Audio) != "raw-pcm" || result.ContentType != "audio/pcm" || result.Format != "pcm" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
