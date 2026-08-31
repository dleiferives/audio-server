package encode

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/dleiferives/audio-server/internal/sttprovider"
)

var pcm16kMono = sttprovider.AudioFormat{
	Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1,
}

func TestNormalizeAudioPassesThroughMatchingWAV(t *testing.T) {
	wav := testWAV(16000, 1, 16)
	f := FFmpeg{Run: func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
		t.Fatal("ffmpeg should not run for matching WAV")
		return nil, nil, nil
	}}

	got, err := f.NormalizeAudio(context.Background(), wav, pcm16kMono)
	if err != nil {
		t.Fatal(err)
	}
	if got.Converted || !reflect.DeepEqual(got.Audio, wav) {
		t.Fatalf("unexpected normalization result: converted=%v audio=%x", got.Converted, got.Audio)
	}
}

func TestNormalizeAudioConvertsArbitraryInput(t *testing.T) {
	var gotArgs []string
	var gotInput []byte
	f := FFmpeg{
		Path: os.Args[0],
		Run: func(_ context.Context, _ string, args []string, input []byte) ([]byte, []byte, error) {
			gotArgs = append([]string(nil), args...)
			gotInput = append([]byte(nil), input...)
			return []byte{1, 2, 3, 4}, nil, nil
		},
	}

	got, err := f.NormalizeAudio(context.Background(), []byte("encoded-audio"), pcm16kMono)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Converted || string(gotInput) != "encoded-audio" {
		t.Fatalf("unexpected result: converted=%v input=%q", got.Converted, gotInput)
	}
	wantArgs := []string{"-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args mismatch:\n got %v\nwant %v", gotArgs, wantArgs)
	}
	if !matchesWAV(got.Audio, pcm16kMono) || !reflect.DeepEqual(got.Audio[44:], []byte{1, 2, 3, 4}) {
		t.Fatalf("normalizer returned invalid WAV: %x", got.Audio)
	}
}

func TestNormalizeAudioMapsDecodeFailureToInvalidRequest(t *testing.T) {
	f := FFmpeg{
		Path: os.Args[0],
		Run: func(context.Context, string, []string, []byte) ([]byte, []byte, error) {
			return nil, []byte("invalid data"), errors.New("exit 1")
		},
	}

	_, err := f.NormalizeAudio(context.Background(), []byte("broken"), pcm16kMono)
	if !errors.Is(err, sttprovider.ErrInvalidRequest) {
		t.Fatalf("expected invalid request, got %v", err)
	}
}

func testWAV(sampleRate, channels, bitsPerSample int) []byte {
	wav := make([]byte, 46)
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	byteRate := sampleRate * channels * bitsPerSample / 8
	binary.LittleEndian.PutUint32(wav[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(wav[32:34], uint16(channels*bitsPerSample/8))
	binary.LittleEndian.PutUint16(wav[34:36], uint16(bitsPerSample))
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], 2)
	return wav
}
