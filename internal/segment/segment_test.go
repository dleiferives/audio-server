package segment

import (
	"strings"
	"testing"
)

func TestPackGroupsToTarget(t *testing.T) {
	sentences := []string{
		"One two three four five.",  // 5
		"Six seven eight nine ten.", // 5
		"Eleven twelve thirteen.",   // 3
	}
	// target=8 closes a chunk rather than overshooting it: the second sentence
	// would take the first chunk to 10 words, so it starts a new one, and the
	// third fits alongside it at exactly 8.
	got := Pack(sentences, 8, 16)
	want := []string{
		"One two three four five.",
		"Six seven eight nine ten. Eleven twelve thirteen.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPackNeverExceedsMax(t *testing.T) {
	// A single sentence far longer than the cap must still be cut.
	long := strings.TrimSpace(strings.Repeat("word ", 150))
	for _, chunk := range Pack([]string{long}, 32, 64) {
		if n := WordCount(chunk); n > 64 {
			t.Errorf("chunk has %d words, want <= 64", n)
		}
	}
}

func TestPackSplitsOversizedSentenceAfterFlushing(t *testing.T) {
	short := "Short one."
	long := strings.TrimSpace(strings.Repeat("word ", 10))
	got := Pack([]string{short, long}, 4, 4)
	if len(got) < 2 {
		t.Fatalf("got %q, want the pending chunk flushed before the split", got)
	}
	if got[0] != short {
		t.Errorf("chunk 0 = %q, want %q", got[0], short)
	}
	for i, chunk := range got {
		if n := WordCount(chunk); n > 4 {
			t.Errorf("chunk %d has %d words, want <= 4", i, n)
		}
	}
}

func TestPackDisabledWithoutMax(t *testing.T) {
	if got := Pack([]string{"Anything at all."}, 32, 0); got != nil {
		t.Errorf("Pack with maxWords=0 = %q, want nil", got)
	}
}

func TestPackTargetAboveMaxClampsToMax(t *testing.T) {
	sentences := []string{"a b c.", "d e f.", "g h i."}
	for i, chunk := range Pack(sentences, 100, 6) {
		if n := WordCount(chunk); n > 6 {
			t.Errorf("chunk %d has %d words, want <= 6", i, n)
		}
	}
}

func TestTerminateAddsMissingSentenceMark(t *testing.T) {
	cases := map[string]string{
		"already done.": "already done.",
		"shouting!":     "shouting!",
		"asking?":       "asking?",
		"no mark here":  "no mark here.",
		"trailing bit ": "trailing bit.",
		`he said "hi."`: `he said "hi.".`, // closing quote hides the mark
		"句号。":           "句号。",
	}
	for in, want := range cases {
		if got := Terminate(in); got != want {
			t.Errorf("Terminate(%q) = %q, want %q", in, got, want)
		}
	}
}
