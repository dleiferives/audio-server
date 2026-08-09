package qwen3asr

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

type fakeClient struct {
	do func(*http.Request) (*http.Response, error)
}

func (f fakeClient) Do(req *http.Request) (*http.Response, error) { return f.do(req) }

func response(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestTranscribeStartsAndSendsConfiguredModel(t *testing.T) {
	starts := 0
	p := New("qwen3-asr-0.6b", "qwen3-asr-0.6b", "http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := req.FormValue("model"); got != "qwen3-asr-0.6b" {
			t.Fatalf("unexpected model %q", got)
		}
		if got := req.FormValue("language"); got != "el" {
			t.Fatalf("unexpected normalized language %q", got)
		}
		return response(http.StatusOK, "application/json", `{"text":"γεια"}`), nil
	}})
	p.StartFunc = func() error {
		starts++
		return nil
	}

	result, err := p.Transcribe(context.Background(), sttprovider.TranscriptionRequest{
		Audio:    []byte("wav"),
		Filename: "sample.wav",
		Language: "el-GR",
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || result.Text != "γεια" || result.ProviderID != "qwen3-asr-0.6b" {
		t.Fatalf("unexpected starts=%d result=%+v", starts, result)
	}
}

func TestTranscribeStreamForwardsEvents(t *testing.T) {
	p := New("qwen3-asr-1.7b", "qwen3-asr-1.7b", "http://sidecar", fakeClient{do: func(req *http.Request) (*http.Response, error) {
		return response(http.StatusOK, "text/event-stream", "data: {\"type\":\"transcript.text.delta\",\"delta\":\"γεια\"}\n\ndata: {\"type\":\"transcript.text.done\",\"text\":\"γεια σου\"}\n\ndata: [DONE]\n\n"), nil
	}})
	var partial string
	result, err := p.TranscribeStream(
		context.Background(),
		sttprovider.TranscriptionRequest{Audio: []byte("wav")},
		func(result sttprovider.TranscriptionResult) error {
			partial = result.Text
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if partial != "γεια" || result.Text != "γεια σου" {
		t.Fatalf("unexpected partial=%q result=%+v", partial, result)
	}
}
