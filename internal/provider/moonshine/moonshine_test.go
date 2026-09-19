package moonshine

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

func TestAudioRequirementsMatchSidecarDecoder(t *testing.T) {
	got := New("http://sidecar", nil).AudioRequirements()
	want := sttprovider.AudioFormat{Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestNormalizeLanguageReducesLocaleTags(t *testing.T) {
	for input, want := range map[string]string{
		"en":      "en",
		"en-US":   "en",
		"el_GR":   "el",
		"  ZH-tw": "zh",
		"":        "",
	} {
		if got := normalizeLanguage(input); got != want {
			t.Errorf("normalizeLanguage(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTranscribeSendsAudioAndLanguageHeader(t *testing.T) {
	var gotPath, gotBody, gotLanguage string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		gotLanguage = req.Header.Get("X-Transcribe-Language")
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return jsonResponse(200, `{"text":"hello world","language":"en","duration":1.5}`), nil
	}})

	result, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio:    []byte("RIFFfake"),
		Language: "en-US",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/transcribe" {
		t.Fatalf("unexpected path %s", gotPath)
	}
	if gotBody != "RIFFfake" {
		t.Fatalf("unexpected body %q", gotBody)
	}
	// Moonshine v2 ships one model per language, so the language has to reach
	// the sidecar rather than being dropped.
	if gotLanguage != "en" {
		t.Fatalf("expected normalized language header, got %q", gotLanguage)
	}
	if result.Text != "hello world" || result.Language != "en" || result.Duration != 1.5 {
		t.Fatalf("unexpected result %+v", result)
	}
	if result.ProviderID != id {
		t.Fatalf("unexpected provider id %q", result.ProviderID)
	}
}

func TestTranscribeOmitsLanguageHeaderWhenUnset(t *testing.T) {
	var present bool
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		_, present = req.Header["X-Transcribe-Language"]
		return jsonResponse(200, `{"text":"ok","language":"en","duration":1}`), nil
	}})
	if _, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("expected no language header when the request does not set one")
	}
}

func TestTranscribeRequiresAudio(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("client should not be called")
		return nil, nil
	}})
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{})
	if !errors.Is(err, sttprovider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestTranscribeSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(400, `{"error":{"message":"expected 16000 Hz audio, got 44100 Hz"}}`), nil
	}})
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("x")})
	if !errors.Is(err, sttprovider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "44100 Hz") {
		t.Fatalf("expected sidecar detail in error, got %v", err)
	}
}

func TestTranscribeStartsSidecarOnDemand(t *testing.T) {
	started := 0
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"text":"ok","language":"en","duration":1}`), nil
	}})
	p.StartFunc = func() error {
		started++
		return nil
	}
	if !p.StartsOnDemand() {
		t.Fatal("expected StartsOnDemand to be true once StartFunc is set")
	}
	if _, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("expected StartFunc to run once, ran %d times", started)
	}
}

func TestTranscribeReportsLifecycleFailure(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("client should not be called when the sidecar cannot start")
		return nil, nil
	}})
	p.StartFunc = func() error { return errors.New("port already bound") }
	_, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{Audio: []byte("x")})
	if !errors.Is(err, sttprovider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

// ── live streaming ──

func TestSupportsLiveStreaming(t *testing.T) {
	if !New("http://sidecar", nil).SupportsLiveStreaming() {
		t.Fatal("moonshine v2 streams; expected SupportsLiveStreaming to be true")
	}
}

func TestOpenLiveStreamRejectsNon16kMono(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		t.Fatal("client should not be called for unsupported audio")
		return nil, nil
	}})
	for _, req := range []sttprovider.LiveStreamRequest{
		{SampleRate: 44100, Channels: 1},
		{SampleRate: 16000, Channels: 2},
	} {
		if _, err := p.OpenLiveStream(context.Background(), req); !errors.Is(err, sttprovider.ErrInvalidRequest) {
			t.Fatalf("%+v: expected ErrInvalidRequest, got %v", req, err)
		}
	}
}

func TestLiveStreamFeedAndFinalize(t *testing.T) {
	var paths []string
	var beginLanguage string
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/stream/begin":
			beginLanguage = req.Header.Get("X-Transcribe-Language")
			return jsonResponse(200, `{"status":"ok"}`), nil
		case "/stream/feed":
			return jsonResponse(200, `{"text":"the quick","committed_text":"","tentative_text":"the quick","input_ms":500,"buffered_ms":500,"revision":1,"changed":true}`), nil
		case "/stream/finalize":
			return jsonResponse(200, `{"text":"the quick brown fox","committed_text":"the quick brown fox","tentative_text":"","input_ms":1000,"buffered_ms":0,"revision":2,"changed":true,"final":true}`), nil
		}
		t.Fatalf("unexpected path %s", req.URL.Path)
		return nil, nil
	}})

	stream, err := p.OpenLiveStream(context.Background(), sttprovider.LiveStreamRequest{
		Language: "en-US", SampleRate: 16000, Channels: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if beginLanguage != "en" {
		t.Fatalf("expected normalized language on begin, got %q", beginLanguage)
	}

	update, err := stream.Feed(context.Background(), []byte{0x00, 0x01, 0x02, 0x03})
	if err != nil {
		t.Fatal(err)
	}
	if update.TentativeText != "the quick" || update.Revision != 1 || !update.Changed {
		t.Fatalf("unexpected feed update %+v", update)
	}
	if update.Final {
		t.Fatal("a feed update must not be final")
	}

	final, err := stream.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.CommittedText != "the quick brown fox" || !final.Final {
		t.Fatalf("unexpected final update %+v", final)
	}

	// Close after Finalize must not issue a reset.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"/stream/begin", "/stream/feed", "/stream/finalize"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected call sequence %v, want %v", paths, want)
	}
}

func TestLiveStreamRejectsOddPCM(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"status":"ok"}`), nil
	}})
	stream, err := p.OpenLiveStream(context.Background(), sttprovider.LiveStreamRequest{SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, pcm := range [][]byte{nil, {0x01}} {
		if _, err := stream.Feed(context.Background(), pcm); !errors.Is(err, sttprovider.ErrInvalidRequest) {
			t.Fatalf("pcm %v: expected ErrInvalidRequest, got %v", pcm, err)
		}
	}
}

func TestLiveStreamCloseResetsSession(t *testing.T) {
	var reset bool
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/stream/reset" {
			reset = true
		}
		return jsonResponse(200, `{"status":"ok"}`), nil
	}})
	stream, err := p.OpenLiveStream(context.Background(), sttprovider.LiveStreamRequest{SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !reset {
		t.Fatal("expected Close to reset the sidecar session")
	}
	// Feeding a closed stream is a client error, not a sidecar round trip.
	if _, err := stream.Feed(context.Background(), []byte{0x00, 0x01}); !errors.Is(err, sttprovider.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest after Close, got %v", err)
	}
}

func TestLiveStreamSurfacesSidecarError(t *testing.T) {
	p := New("http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/stream/begin" {
			return jsonResponse(200, `{"status":"ok"}`), nil
		}
		return jsonResponse(409, `{"error":{"message":"no live session; POST /stream/begin first"}}`), nil
	}})
	stream, err := p.OpenLiveStream(context.Background(), sttprovider.LiveStreamRequest{SampleRate: 16000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Feed(context.Background(), []byte{0x00, 0x01})
	if !errors.Is(err, sttprovider.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("expected sidecar message in error, got %v", err)
	}
}
