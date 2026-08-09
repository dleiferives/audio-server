package parakeet

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
	p := New("http://sidecar/", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/health" {
			t.Fatalf("path = %q, want /health", req.URL.Path)
		}
		return jsonResponse(http.StatusOK, `{"status":"ok"}`), nil
	}})
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeStartsSidecarAndSendsMultipartRequest(t *testing.T) {
	started := false
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if !started {
			t.Fatal("sidecar request made before lifecycle start")
		}
		if req.URL.Path != "/v1/audio/transcriptions" {
			t.Fatalf("path = %q, want transcription endpoint", req.URL.Path)
		}
		reader, err := req.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			fields[part.FormName()] = string(value)
		}
		if fields["model"] != "parakeet" || fields["language"] != "en" || fields["file"] != "fake-wav" {
			t.Fatalf("unexpected multipart fields: %#v", fields)
		}
		return jsonResponse(http.StatusOK, `{"text":"hello world"}`), nil
	}})
	p.StartFunc = func() error {
		started = true
		return nil
	}

	result, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio:    []byte("fake-wav"),
		Filename: "speech.wav",
		Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello world" || result.ProviderID != "parakeet" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestTranscribeRejectsEmptyAudioBeforeStarting(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(*http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}})
	p.StartFunc = func() error {
		t.Fatal("should not start sidecar")
		return nil
	}
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{})
	if !errors.Is(err, sttprovider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestTranscribeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError, `{"error":{"message":"model failed"}}`), nil
	}})
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("wav")})
	if !errors.Is(err, sttprovider.ErrUnavailable) || !strings.Contains(err.Error(), "model failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranscribeStreamForwardsPartialSnapshots(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		reader, err := req.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		var streamField string
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if part.FormName() == "stream" {
				streamField = string(value)
			}
		}
		if streamField != "true" {
			t.Fatalf("stream field = %q, want true", streamField)
		}
		return jsonResponse(http.StatusOK, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n"+
			"data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello world\"}\n\n"+
			"data: {\"type\":\"transcript.text.done\",\"text\":\"hello world\"}\n\n"+
			"data: [DONE]\n\n"), nil
	}})

	var partials []string
	result, err := p.TranscribeStream(
		context.Background(),
		sttprovider.TranscriptionRequest{Audio: []byte("wav"), Filename: "audio.wav"},
		func(partial sttprovider.TranscriptionResult) error {
			partials = append(partials, partial.Text)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(partials, "|") != "hello|hello world" {
		t.Fatalf("partials = %#v", partials)
	}
	if result.Text != "hello world" || result.ProviderID != "parakeet" {
		t.Fatalf("unexpected final result: %+v", result)
	}
}
