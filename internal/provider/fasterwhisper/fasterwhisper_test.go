package fasterwhisper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/sttprovider"
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
	if err := p.Health(context.Background()); !errors.Is(err, sttprovider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestTranscribeSendsAudioAndLanguage(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		gotQuery = req.URL.RawQuery
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return jsonResponse(200, `{"text":"hello world","language":"en","duration":1.5}`), nil
	}})

	result, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio:    []byte("fake-wav-bytes"),
		Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/transcribe" || gotQuery != "language=en" || gotBody != "fake-wav-bytes" {
		t.Fatalf("unexpected request: path=%q query=%q body=%q", gotPath, gotQuery, gotBody)
	}
	if result.Text != "hello world" || result.Language != "en" || result.Duration != 1.5 || result.ProviderID != "faster-whisper" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestTranscribeNormalizesBrowserLocale(t *testing.T) {
	var gotLanguage string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		gotLanguage = req.URL.Query().Get("language")
		return jsonResponse(200, `{"text":"γεια","language":"el","duration":1}`), nil
	}})

	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio:    []byte("fake-wav-bytes"),
		Language: "el-GR",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotLanguage != "el" {
		t.Fatalf("expected normalized language el, got %q", gotLanguage)
	}
}

func TestTranscribeStartsLifecycleManagedSidecar(t *testing.T) {
	starts := 0
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"text":"hello","language":"en","duration":1}`), nil
	}})
	p.StartFunc = func() error {
		starts++
		return nil
	}

	if !p.StartsOnDemand() {
		t.Fatal("expected provider to report on-demand startup")
	}
	if _, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("wav")}); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("expected one lifecycle start, got %d", starts)
	}
}

func TestTranscribeStreamEmitsBufferedResult(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"text":"hello world","language":"en","duration":1.5}`), nil
	}})

	var partial sttprovider.TranscriptionResult
	result, err := p.TranscribeStream(
		context.Background(),
		sttprovider.TranscriptionRequest{Audio: []byte("wav")},
		func(got sttprovider.TranscriptionResult) error {
			partial = got
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Text != "hello world" || result.Text != "hello world" {
		t.Fatalf("unexpected partial=%+v result=%+v", partial, result)
	}
}

func TestTranscribeRejectsEmptyAudio(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}})
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{})
	if !errors.Is(err, sttprovider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestTranscribeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"error":"boom"}`), nil
	}})
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("x")})
	if !errors.Is(err, sttprovider.ErrUnavailable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}
