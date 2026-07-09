package kokoro

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

func TestWarmCallsLoad(t *testing.T) {
	var gotPath, gotMethod string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		gotPath, gotMethod = req.URL.Path, req.Method
		return jsonResponse(200, `{"status":"ok"}`), nil
	}}, nil)
	if err := p.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/load" || gotMethod != http.MethodPost {
		t.Fatalf("unexpected request: %s %s", gotMethod, gotPath)
	}
}

func TestIdleCallsUnload(t *testing.T) {
	var gotPath string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		return jsonResponse(200, `{"status":"ok"}`), nil
	}}, nil)
	if err := p.Idle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/unload" {
		t.Fatalf("unexpected path %s", gotPath)
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

func TestVoicesFiltersByLanguage(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"voices":[{"provider":"kokoro","voice":"af_heart","language":"en-us","name":"American English (female)"}]}`), nil
	}}, nil)
	voices, err := p.Voices(context.Background(), "EN-US")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 1 || voices[0].Provider != "kokoro" || voices[0].Voice != "af_heart" {
		t.Fatalf("unexpected voices: %+v", voices)
	}

	voices, err = p.Voices(context.Background(), "en-gb")
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 0 {
		t.Fatalf("expected no voices for en-gb, got %+v", voices)
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
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("RIFF..."))}, nil
	}}, nil)

	result, err := p.Synthesize(context.Background(), provider.SpeechRequest{
		Input:    "hello there",
		Voice:    "am_adam",
		Language: "EN-GB",
		Speed:    1.2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Audio) != "RIFF..." || result.ContentType != "audio/wav" || result.ProviderID != "kokoro" || result.Voice != "am_adam" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotBody.Text != "hello there" || gotBody.Voice != "am_adam" || gotBody.Language != "en-gb" || gotBody.Speed != 1.2 {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestSynthesizeDefaultsVoiceAndLanguage(t *testing.T) {
	var gotBody synthesizeRequest
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		json.Unmarshal(body, &gotBody)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("RIFF..."))}, nil
	}}, nil)

	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Voice != defaultVoice || gotBody.Language != defaultLang {
		t.Fatalf("unexpected defaults: %+v", gotBody)
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

func TestSynthesizeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"error":"boom"}`), nil
	}}, nil)
	_, err := p.Synthesize(context.Background(), provider.SpeechRequest{Input: "hi"})
	if !errors.Is(err, provider.ErrUnavailable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWarmSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(503, `{"error":"out of memory"}`), nil
	}}, nil)
	err := p.Warm(context.Background())
	if !errors.Is(err, provider.ErrUnavailable) || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("unexpected error: %v", err)
	}
}
