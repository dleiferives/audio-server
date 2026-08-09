package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/sttprovider"
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

func TestSpeechValidatesVoiceAgainstLanguage(t *testing.T) {
	var synthesized bool
	s := newTestServer(t, fakeProvider{
		voicesHook: func(_ context.Context, language string) ([]provider.Voice, error) {
			if language != "el" {
				t.Fatalf("language = %q, want el", language)
			}
			return []provider.Voice{{Provider: "fake", Voice: "F1", Language: "el", Name: "Female 1"}}, nil
		},
		synthesizeHook: func(provider.SpeechRequest) { synthesized = true },
	}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","language":"el","voice":"en-us"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if synthesized {
		t.Fatal("provider synthesized a voice that is not available for the language")
	}
}

func TestSpeechDefaultsVoiceToAutoForLanguage(t *testing.T) {
	var got provider.SpeechRequest
	s := newTestServer(t, fakeProvider{
		voicesHook: func(_ context.Context, language string) ([]provider.Voice, error) {
			if language != "el" {
				t.Fatalf("language = %q, want el", language)
			}
			return []provider.Voice{{Provider: "fake", Voice: "F1", Language: "el", Name: "Female 1"}}, nil
		},
		synthesizeHook: func(req provider.SpeechRequest) { got = req },
	}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","language":"el"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Voice != "auto" || got.Language != "el" {
		t.Fatalf("request = %+v, want voice auto and language el", got)
	}
}

func TestSpeechDefaultsLanguageToAuto(t *testing.T) {
	var got provider.SpeechRequest
	s := newTestServer(t, fakeProvider{
		synthesizeHook: func(req provider.SpeechRequest) { got = req },
	}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Language != "auto" || got.Voice != "auto" {
		t.Fatalf("request = %+v, want language and voice auto", got)
	}
}

func TestSpeechValidation(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})
	tests := []string{
		`{"input":""}`,
		`{"input":"hello","response_format":"aac"}`,
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

func TestHealthReportsColdLifecycleProviderAsConfigured(t *testing.T) {
	p := fakeManagedProvider{fakeProvider: fakeProvider{id: "managed", healthErr: provider.ErrUnavailable}}
	s := newTestServerWithProvider(t, p, "managed", Config{})
	resp := request(t, s, http.MethodGet, "/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a cold managed provider", resp.StatusCode)
	}
	var body struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Providers["managed"] != "cold" {
		t.Fatalf("unexpected health body: %+v", body)
	}
}

func TestHealthReportsColdOnDemandSttProviderAsConfigured(t *testing.T) {
	stt := fakeSttProvider{
		id:        "parakeet",
		healthErr: sttprovider.ErrUnavailable,
		onDemand:  true,
	}
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{stt}})
	resp := request(t, s, http.MethodGet, "/healthz", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a cold on-demand STT provider", resp.StatusCode)
	}
	var body struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Providers["parakeet"] != "cold" {
		t.Fatalf("unexpected health body: %+v", body)
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

func TestStreamSpeechUsesStreamer(t *testing.T) {
	p := fakeStreamer{fakeProvider: fakeProvider{id: "fake"}}
	s := newTestServerWithProvider(t, p, "fake", Config{})

	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","stream":true}`, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "streamed-bytes" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-TTS-Provider") != "fake" || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("unexpected headers: %v", resp.Header)
	}
}

func TestLifecycleStreamSpeechUsesQueueAndStreamer(t *testing.T) {
	p := fakeManagedStreamer{fakeStreamer: fakeStreamer{fakeProvider: fakeProvider{id: "managed"}}}
	s := newTestServerWithProvider(t, p, "managed", Config{})

	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","stream":true}`, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "streamed-bytes" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-TTS-Provider") != "managed" || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("unexpected headers: %v", resp.Header)
	}
}

func TestStreamSpeechFallsBackWhenProviderCannotStream(t *testing.T) {
	s := newTestServer(t, fakeProvider{id: "fake"}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/speech", `{"input":"hello","stream":true}`, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("expected buffered fallback, got status=%d body=%q", resp.StatusCode, body)
	}
}

func TestCreateJobRejectsStream(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})
	resp := request(t, s, http.MethodPost, "/v1/audio/jobs", `{"input":"hello","stream":true}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTranscriptionsWithoutProvidersReturns503(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{})
	resp := multipartAudioRequest(t, s, nil, []byte("fake-audio"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestTranscriptionsReturnsText(t *testing.T) {
	var gotAudio []byte
	var gotLanguage string
	stt := fakeSttProvider{
		id:     "faster-whisper",
		result: sttprovider.TranscriptionResult{Text: "hello world", Language: "en"},
		transcribeHook: func(req sttprovider.TranscriptionRequest) {
			gotAudio = req.Audio
			gotLanguage = req.Language
		},
	}
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{stt}})

	resp := multipartAudioRequest(t, s, map[string]string{"language": "en"}, []byte("fake-audio-bytes"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "hello world" {
		t.Fatalf("unexpected text: %q", body.Text)
	}
	if string(gotAudio) != "fake-audio-bytes" || gotLanguage != "en" {
		t.Fatalf("unexpected request: audio=%q language=%q", gotAudio, gotLanguage)
	}
}

func TestTranscriptionsPlainTextFormat(t *testing.T) {
	stt := fakeSttProvider{id: "faster-whisper", result: sttprovider.TranscriptionResult{Text: "plain text result"}}
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{stt}})

	resp := multipartAudioRequest(t, s, map[string]string{"response_format": "text"}, []byte("audio"))
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "plain text result" {
		t.Fatalf("unexpected response: status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("unexpected content-type: %q", ct)
	}
}

func TestTranscriptionsStreamsPartialText(t *testing.T) {
	stt := fakeStreamingSttProvider{
		fakeSttProvider: fakeSttProvider{id: "parakeet"},
		partials:        []string{"hello", "hello world"},
		final:           sttprovider.TranscriptionResult{Text: "hello world", ProviderID: "parakeet"},
	}
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{stt}})

	resp := multipartAudioRequest(t, s, map[string]string{"stream": "true"}, []byte("audio"))
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(text, `"type":"transcript.text.delta"`) ||
		!strings.Contains(text, `"delta":"hello world"`) ||
		!strings.Contains(text, `"type":"transcript.text.done"`) ||
		!strings.Contains(text, "data: [DONE]") {
		t.Fatalf("unexpected streaming response: status=%d headers=%v body=%q", resp.StatusCode, resp.Header, text)
	}
}

func TestTranscriptionsUsesConfiguredDefaultProvider(t *testing.T) {
	var parakeetCalled bool
	parakeet := fakeSttProvider{
		id:     "parakeet",
		result: sttprovider.TranscriptionResult{Text: "primary"},
		transcribeHook: func(sttprovider.TranscriptionRequest) {
			parakeetCalled = true
		},
	}
	nemotron := fakeSttProvider{
		id:     "nemotron",
		result: sttprovider.TranscriptionResult{Text: "fallback"},
		transcribeHook: func(sttprovider.TranscriptionRequest) {
			t.Fatal("non-default STT provider was called")
		},
	}
	s := newTestServer(t, fakeProvider{}, Config{
		SttProviders:       []sttprovider.Provider{nemotron, parakeet},
		DefaultSttProvider: "parakeet",
	})

	resp := multipartAudioRequest(t, s, map[string]string{"model": "auto"}, []byte("audio"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !parakeetCalled {
		t.Fatal("configured default STT provider was not called")
	}
}

func TestTranscriptionsUnknownModelReturns400(t *testing.T) {
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{fakeSttProvider{id: "faster-whisper"}}})
	resp := multipartAudioRequest(t, s, map[string]string{"model": "nonexistent"}, []byte("audio"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTranscriptionsProviderErrorMapsTo503(t *testing.T) {
	stt := fakeSttProvider{id: "faster-whisper", transcribeErr: sttprovider.ErrUnavailable}
	s := newTestServer(t, fakeProvider{}, Config{SttProviders: []sttprovider.Provider{stt}})
	resp := multipartAudioRequest(t, s, nil, []byte("audio"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
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

func TestCapabilities(t *testing.T) {
	s := newTestServer(t, fakeProvider{
		voices: []provider.Voice{
			{Provider: "fake", Voice: "el-1", Language: "el", Name: "Greek"},
			{Provider: "fake", Voice: "en-1", Language: "en/en-us", Name: "English"},
		},
	}, Config{})
	resp := request(t, s, http.MethodGet, "/v1/audio/capabilities", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Languages []string `json:"languages"`
		Providers []struct {
			Provider  string           `json:"provider"`
			Languages []string         `json:"languages"`
			Voices    []provider.Voice `json:"voices"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.Languages, ",") != "auto,el,en,en-us" || len(body.Providers) != 1 || len(body.Providers[0].Voices) != 2 {
		t.Fatalf("unexpected capabilities: %+v", body)
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

func newTestServerWithProvider(t *testing.T, p provider.Provider, id string, cfg Config) *Server {
	t.Helper()
	cfg.Providers = []provider.Provider{p}
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = id
	}
	cfg.Queue = queue.NewManager(queue.Config{
		Providers: map[string]provider.Provider{id: p},
		Workers:   map[string]int{id: 2},
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
	voicesHook     func(context.Context, string) ([]provider.Voice, error)
}

func (f fakeProvider) ID() string {
	return f.id
}

func (f fakeProvider) SupportsAutoLanguage() bool { return true }

func (f fakeProvider) Health(context.Context) error {
	return f.healthErr
}

func (f fakeProvider) Voices(ctx context.Context, language string) ([]provider.Voice, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	if f.voicesHook != nil {
		return f.voicesHook(ctx, language)
	}
	if f.voices == nil && language == "auto" {
		return []provider.Voice{{Provider: f.id, Voice: "v", Language: "auto", Name: "Default"}}, nil
	}
	return f.voices, nil
}

type fakeStreamer struct {
	fakeProvider
}

type fakeManagedProvider struct {
	fakeProvider
}

type fakeManagedStreamer struct {
	fakeStreamer
}

func (fakeManagedProvider) Warm(context.Context) error { return nil }
func (fakeManagedProvider) Idle(context.Context) error { return nil }
func (fakeManagedStreamer) Warm(context.Context) error { return nil }
func (fakeManagedStreamer) Idle(context.Context) error { return nil }

func (f fakeStreamer) SynthesizeStream(_ context.Context, req provider.SpeechRequest, onHeader func(provider.StreamMeta), w io.Writer) error {
	onHeader(provider.StreamMeta{ProviderID: f.id, Model: f.id, Voice: "v", Format: "wav", ContentType: "audio/wav"})
	_, err := w.Write([]byte("streamed-bytes"))
	return err
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

type fakeSttProvider struct {
	id             string
	result         sttprovider.TranscriptionResult
	healthErr      error
	onDemand       bool
	transcribeErr  error
	transcribeHook func(sttprovider.TranscriptionRequest)
}

type fakeStreamingSttProvider struct {
	fakeSttProvider
	partials []string
	final    sttprovider.TranscriptionResult
}

func (f fakeStreamingSttProvider) TranscribeStream(
	_ context.Context,
	_ sttprovider.TranscriptionRequest,
	onPartial func(sttprovider.TranscriptionResult) error,
) (sttprovider.TranscriptionResult, error) {
	for _, text := range f.partials {
		if err := onPartial(sttprovider.TranscriptionResult{Text: text, ProviderID: f.id}); err != nil {
			return sttprovider.TranscriptionResult{}, err
		}
	}
	return f.final, nil
}

func (f fakeSttProvider) ID() string { return f.id }

func (f fakeSttProvider) Health(context.Context) error { return f.healthErr }

func (f fakeSttProvider) StartsOnDemand() bool { return f.onDemand }

func (f fakeSttProvider) Transcribe(_ context.Context, req sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	if f.transcribeHook != nil {
		f.transcribeHook(req)
	}
	if f.transcribeErr != nil {
		return sttprovider.TranscriptionResult{}, f.transcribeErr
	}
	return f.result, nil
}

func multipartAudioRequest(t *testing.T, s *Server, fields map[string]string, audio []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := w.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr.Result()
}
