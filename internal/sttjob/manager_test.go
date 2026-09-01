package sttjob

import (
	"context"
	"testing"
	"time"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

type fakeProvider struct{}

func (fakeProvider) ID() string                   { return "refine" }
func (fakeProvider) Health(context.Context) error { return nil }
func (fakeProvider) Transcribe(context.Context, sttprovider.TranscriptionRequest) (sttprovider.TranscriptionResult, error) {
	return sttprovider.TranscriptionResult{Text: "refined", ProviderID: "refine"}, nil
}

func TestSubmitAndGet(t *testing.T) {
	manager := New(Config{Providers: map[string]sttprovider.Provider{"refine": fakeProvider{}}})
	job, err := manager.Submit("refine", "live", sttprovider.TranscriptionRequest{Audio: []byte("wav")})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err = manager.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == StatusSucceeded {
			if job.Result.Text != "refined" || job.SourceModel != "live" {
				t.Fatalf("job = %+v", job)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job did not complete: %+v", job)
}
