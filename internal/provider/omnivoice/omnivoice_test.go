package omnivoice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/provider"
)

type fakeClient struct {
	do func(req *http.Request) (*http.Response, error)
}

func (f fakeClient) Do(req *http.Request) (*http.Response, error) {
	return f.do(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestHealthOK(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/health" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		return jsonResponse(200, `{"status":"ok"}`), nil
	}})
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthUnavailable(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}})
	if err := p.Health(context.Background()); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestVoicesFiltersByLanguage(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"voices":[{"provider":"omnivoice","voice":"auto","language":"el","name":"OmniVoice automatic"}]}`), nil
	}})
	voices, err := p.Voices(context.Background(), "EL")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 1 || voices[0].Provider != "omnivoice" || voices[0].Language != "el" {
		t.Fatalf("unexpected voices: %+v", voices)
	}

	voices, err = p.Voices(context.Background(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 0 {
		t.Fatalf("expected no voices for en, got %+v", voices)
	}
}

func TestSynthesizeSendsRequestAndReturnsWAV(t *testing.T) {
	var gotBody synthesizeRequest
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/synthesize" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}},
			Body:       io.NopCloser(strings.NewReader("RIFF...")),
		}, nil
	}})

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:    "γεια σου",
		Language: "EL",
		Speed:    1.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "RIFF..." || result.ContentType != "audio/wav" || result.ProviderID != "omnivoice" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotBody.Text != "γεια σου" || gotBody.Language != "el" || gotBody.Speed != 1.5 {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestSynthesizeRejectsNonWAVFormat(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}})
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi", ResponseFormat: "mp3"})
	if !errors.Is(err, provider.ErrUnsupportedFormat) {
		t.Fatalf("expected ErrUnsupportedFormat, got %v", err)
	}
}

func TestSynthesizeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"error":"boom"}`), nil
	}})
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi"})
	if !errors.Is(err, provider.ErrUnavailable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}
