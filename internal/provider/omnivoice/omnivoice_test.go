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
	}}, nil)
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthUnavailable(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}}, nil)
	if err := p.Health(context.Background()); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestVoicesReturnsList(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/audio/voices" || req.URL.Query().Get("model") != "omnivoice" {
			t.Fatalf("unexpected request: %s?%s", req.URL.Path, req.URL.RawQuery)
		}
		return jsonResponse(200, `{"voices":["auto","preset1"]}`), nil
	}}, nil)
	voices, err := p.Voices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 2 || voices[0].Voice != "auto" || voices[1].Voice != "preset1" {
		t.Fatalf("unexpected voices: %+v", voices)
	}
}

func TestVoicesFallsBackToCatalogWhenSidecarIsCold(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}}, nil)
	voices, err := p.Voices(context.Background(), "el")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 1 || voices[0].Voice != "auto" || voices[0].Language != "el" {
		t.Fatalf("unexpected cold catalog: %+v", voices)
	}
}

func TestSynthesizeSendsRequestAndReturnsWAV(t *testing.T) {
	var gotBody speechRequest
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/audio/speech" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("RIFF...")),
		}, nil
	}}, nil)

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:    "hello",
		Language: "en",
		Speed:    1.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "RIFF..." || result.ContentType != "audio/wav" || result.ProviderID != "omnivoice" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotBody.Input != "hello" || gotBody.Language != "en" || gotBody.Speed != 1.5 {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
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

func TestSynthesizeAppliesProviderOptions(t *testing.T) {
	var gotBody speechRequest
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("RIFF..."))}, nil
	}}, nil)

	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:           "hi",
		ProviderOptions: json.RawMessage(`{"steps":8,"instruct":"female, whisper","guidance_scale":3.0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := gotBody.Options["num_inference_steps"]
	if steps != float64(8) {
		t.Fatalf("expected num_inference_steps=8, got %v", steps)
	}
	if gotBody.Options["instruct"] != "female, whisper" {
		t.Fatalf("expected instruct, got %v", gotBody.Options["instruct"])
	}
	if gotBody.Options["guidance_scale"] != 3.0 {
		t.Fatalf("expected guidance_scale=3.0, got %v", gotBody.Options["guidance_scale"])
	}
}

func TestSynthesizeRejectsUnknownProviderOptionField(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}}, nil)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:           "hi",
		ProviderOptions: json.RawMessage(`{"stpes":8}`),
	})
	if !errors.Is(err, provider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestSynthesizeRejectsInvalidProviderOptionValues(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}}, nil)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:           "hi",
		ProviderOptions: json.RawMessage(`{"steps":0}`),
	})
	if !errors.Is(err, provider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestSynthesizeRejectsNonWAVFormatWithoutEncoder(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("should not call sidecar")
		return nil, nil
	}}, nil)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi", ResponseFormat: "mp3"})
	if !errors.Is(err, provider.ErrUnsupportedFormat) {
		t.Fatalf("expected ErrUnsupportedFormat, got %v", err)
	}
}

type fakeEncoder struct {
	audio       []byte
	contentType string
	err         error
	gotWAV      []byte
	gotFormat   string
}

func (f *fakeEncoder) Encode(_ context.Context, wav []byte, format string) ([]byte, string, error) {
	f.gotWAV = wav
	f.gotFormat = format
	return f.audio, f.contentType, f.err
}

func TestSynthesizeEncodesNonWAVFormatViaEncoder(t *testing.T) {
	enc := &fakeEncoder{audio: []byte("mp3-bytes"), contentType: "audio/mpeg"}
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("RIFF..."))}, nil
	}}, enc)

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi", ResponseFormat: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "mp3-bytes" || result.ContentType != "audio/mpeg" || result.Format != "mp3" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if string(enc.gotWAV) != "RIFF..." || enc.gotFormat != "mp3" {
		t.Fatalf("encoder not called with expected args: wav=%q format=%q", enc.gotWAV, enc.gotFormat)
	}
}

func TestSynthesizeSurfacesEncoderError(t *testing.T) {
	enc := &fakeEncoder{err: provider.ErrUnsupportedFormat}
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("RIFF..."))}, nil
	}}, enc)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi", ResponseFormat: "flac"})
	if !errors.Is(err, provider.ErrUnsupportedFormat) {
		t.Fatalf("expected ErrUnsupportedFormat, got %v", err)
	}
}

func TestSynthesizeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"error":{"message":"boom"}}`), nil
	}}, nil)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi"})
	if !errors.Is(err, provider.ErrUnavailable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}
