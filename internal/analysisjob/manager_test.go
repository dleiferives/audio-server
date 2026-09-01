package analysisjob

import (
	"testing"

	"github.com/dleiferives/audio-server/internal/analysisprovider"
)

func TestMergeTurnsPreservesOverlap(t *testing.T) {
	segments := mergeTurns([]analysisprovider.SpeakerTurn{
		{StartSample: 0, EndSample: 32000, SpeakerID: "spk_00", Confidence: .9},
		{StartSample: 16000, EndSample: 48000, SpeakerID: "spk_01", Confidence: .8},
	}, 16000)
	if len(segments) != 3 {
		t.Fatalf("got %d segments: %#v", len(segments), segments)
	}
	if !segments[1].Overlap || len(segments[1].SpeakerIDs) != 2 || segments[1].StartMS != 1000 || segments[1].EndMS != 2000 {
		t.Fatalf("unexpected overlap: %#v", segments[1])
	}
}
