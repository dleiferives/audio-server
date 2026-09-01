package pcmwav

import "testing"

func TestRoundTrip(t *testing.T) {
	want := Audio{SampleRate: 16000, Samples: []int16{-32768, -1, 0, 1, 32767}}
	data, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.SampleRate != want.SampleRate || len(got.Samples) != len(want.Samples) {
		t.Fatalf("got rate=%d samples=%d", got.SampleRate, len(got.Samples))
	}
	for i := range want.Samples {
		if got.Samples[i] != want.Samples[i] {
			t.Fatalf("sample %d = %d, want %d", i, got.Samples[i], want.Samples[i])
		}
	}
}
