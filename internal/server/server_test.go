package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/queue"
)

func TestSpeechReturnsProviderAudio(t *testing.T) {
	s := newTestServer(t, fakeProvider{
		result: provider.SpeechResult{
			Audio:       []byte("audio"),
			ContentType: "audio/mpeg",
			ProviderID:  "fake",
			Model:       "fake",
			Voice:       "v1",
			Format:      "mp3",
		},
	}, Config{})

	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"model":"tts-1","input":"hello","voice":"auto"}`, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "audio" {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("X-TTS-Provider") != "fake" {
		t.Fatalf("unexpected headers: %v", resp.Header)
	}
}

func TestSpeechValidation(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})
	tests := []string{
		`{"input":""}`,
		`{"input":"hello","response_format":"opus"}`,
		`{"input":"hello","speed":0.1}`,
		`{"input":"hello","extra":true}`,
	}
	for _, body := range tests {
		resp := request(t, s, http.MethodPost, "/v1/audio/speech", body, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestSpeechPassesProviderOptionsThrough(t *testing.T) {
	var gotOptions string
	p := fakeProvider{synthesizeHook: func(req provider.SpeechRequest) {
		gotOptions = string(req.ProviderOptions)
	}}
	s := newTestServer(t, p, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","provider_options":{"steps":8}}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotOptions != `{"steps":8}` {
		t.Fatalf("provider_options not passed through, got %q", gotOptions)
	}
}

func TestSpeechInvalidProviderOptionsReturns400(t *testing.T) {
	s := newTestServer(t, fakeProvider{synthesizeErr: provider.ErrInvalidRequest}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","provider_options":{"steps":0}}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSpeechInputLengthLimit(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{MaxInputChars: 3})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"four"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{APIKey: "secret"})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", resp.StatusCode)
	}

	resp = request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello"}`, "secret")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthReportsProviderFailure(t *testing.T) {
	s := newTestServer(t, fakeProvider{healthErr: provider.ErrUnavailable}, Config{})
	resp := request(t, s, http.MethodGet, "/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestProviderTimeout(t *testing.T) {
	// The job keeps running in the background even after the waiting HTTP
	// request times out — a slow provider no longer gets its Synthesize call
	// cancelled just because one caller stopped waiting.
	s := newTestServer(t, fakeProvider{delay: 50 * time.Millisecond}, Config{RequestTimeout: time.Millisecond})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
}

func TestJobLifecycle(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})

	resp := request(t, s, http.MethodPost, "/v1/audio/jobs", `{"input":"hello"}`, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("expected a job id")
	}

	var status string
	for i := 0; i < 100; i++ {
		r := request(t, s, http.MethodGet, "/v1/audio/jobs/"+created.ID, "", "")
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		r.Body.Close()
		status = body.Status
		if status == "succeeded" || status == "failed" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if status != "succeeded" {
		t.Fatalf("job status = %q, want succeeded", status)
	}

	audioResp := request(t, s, http.MethodGet, "/v1/audio/jobs/"+created.ID+"/audio", "", "")
	defer audioResp.Body.Close()
	body, _ := io.ReadAll(audioResp.Body)
	if audioResp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("unexpected audio response: status=%d body=%q", audioResp.StatusCode, body)
	}
}

func TestJobUnknownIDReturns404(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})
	resp := request(t, s, http.MethodGet, "/v1/audio/jobs/nope", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	resp = request(t, s, http.MethodGet, "/v1/audio/jobs/nope/audio", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("audio status = %d, want 404", resp.StatusCode)
	}
}

func TestJobAudioNotReadyReturns409(t *testing.T) {
	s := newTestServer(t, fakeProvider{delay: 50 * time.Millisecond}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/jobs", `{"input":"hello"}`, "")
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	audioResp := request(t, s, http.MethodGet, "/v1/audio/jobs/"+created.ID+"/audio", "", "")
	audioResp.Body.Close()
	if audioResp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", audioResp.StatusCode)
	}
}

func TestVoices(t *testing.T) {
	s := newTestServer(t, fakeProvider{
		voices: []provider.Voice{{Provider: "fake", Voice: "en", Language: "en", Name: "English"}},
	}, Config{})
	resp := request(t, s, http.MethodGet, "/v1/audio/voices?language=en", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Voices []provider.Voice `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Voices) != 1 || body.Voices[0].Voice != "en" {
		t.Fatalf("unexpected voices: %+v", body.Voices)
	}
}

func newTestServer(t *testing.T, p fakeProvider, cfg Config) *Server {
	t.Helper()
	if p.id == "" {
		p.id = "fake"
	}
	if len(p.result.Audio) == 0 {
		p.result = provider.SpeechResult{Audio: []byte("ok"), ContentType: "audio/mpeg", ProviderID: p.id, Model: p.id, Voice: "v", Format: "mp3"}
	}
	cfg.Providers = []provider.Provider{p}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = p.id
	}
	cfg.Queue = queue.NewManager(queue.Config{
		Providers: map[string]provider.Provider{p.id: p},
		Workers:   map[string]int{p.id: 2},
	})
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(t *testing.T, s *Server, method, path, body, key string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr.Result()
}

type fakeProvider struct {
	id             string
	result         provider.SpeechResult
	voices         []provider.Voice
	healthErr      error
	synthesizeErr  error
	delay          time.Duration
	synthesizeHook func(provider.SpeechRequest)
}

func (f fakeProvider) ID() string {
	return f.id
}

func (f fakeProvider) Health(context.Context) error {
	return f.healthErr
}

func (f fakeProvider) Voices(context.Context, string) ([]provider.Voice, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return f.voices, nil
}

func (f fakeProvider) Synthesize(ctx context.Context, req provider.SpeechRequest) (provider.SpeechResult, error) {
	if f.synthesizeHook != nil {
		f.synthesizeHook(req)
	}
	if strings.TrimSpace(req.ResponseFormat) == "" {
		return provider.SpeechResult{}, errors.New("response_format was not normalized")
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.synthesizeErr != nil {
		return provider.SpeechResult{}, f.synthesizeErr
	}
	return f.result, nil
}
