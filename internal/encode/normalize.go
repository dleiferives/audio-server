package encode

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/sttprovider"
)

// NormalizeReferenceAudio decodes an uploaded TTS reference into the WAV
// format accepted by native audio.cpp voice-cloning providers.
func (f FFmpeg) NormalizeReferenceAudio(ctx context.Context, audio []byte) ([]byte, error) {
	normalized, err := f.NormalizeAudio(ctx, audio, sttprovider.AudioFormat{
		Container: "wav", Codec: "pcm_s16le", SampleRate: 24000, Channels: 1,
	})
	if err == nil {
		return normalized.Audio, nil
	}
	switch {
	case errors.Is(err, sttprovider.ErrInvalidRequest):
		return nil, fmt.Errorf("%w: invalid speaker reference audio: %v", provider.ErrInvalidRequest, err)
	case errors.Is(err, sttprovider.ErrUnsupportedFormat):
		return nil, fmt.Errorf("%w: speaker reference audio: %v", provider.ErrUnsupportedFormat, err)
	default:
		return nil, fmt.Errorf("%w: speaker reference normalization failed: %v", provider.ErrUnavailable, err)
	}
}

// NormalizeAudio converts an upload to a provider's canonical input format.
// The current STT providers use mono 16-bit PCM WAV at 16 kHz, but the command
// is built from AudioFormat so future providers can declare other PCM rates and
// channel counts without changing the HTTP handler.
func (f FFmpeg) NormalizeAudio(ctx context.Context, audio []byte, format sttprovider.AudioFormat) (sttprovider.NormalizedAudio, error) {
	if len(audio) == 0 {
		return sttprovider.NormalizedAudio{}, fmt.Errorf("%w: audio is empty", sttprovider.ErrInvalidRequest)
	}
	if matchesWAV(audio, format) {
		return sttprovider.NormalizedAudio{Audio: audio}, nil
	}
	if !strings.EqualFold(format.Container, "wav") || !strings.EqualFold(format.Codec, "pcm_s16le") || format.SampleRate <= 0 || format.Channels <= 0 {
		return sttprovider.NormalizedAudio{}, fmt.Errorf("%w: unsupported normalization target %+v", sttprovider.ErrUnsupportedFormat, format)
	}
	if _, err := exec.LookPath(f.path()); err != nil {
		return sttprovider.NormalizedAudio{}, fmt.Errorf("%w: %s not found", sttprovider.ErrUnavailable, f.path())
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-i", "pipe:0",
		"-ar", fmt.Sprintf("%d", format.SampleRate),
		"-ac", fmt.Sprintf("%d", format.Channels),
		"-c:a", "pcm_s16le",
		"-f", "s16le",
		"pipe:1",
	}
	pcm, stderr, err := f.runner()(ctx, f.path(), args, audio)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return sttprovider.NormalizedAudio{}, err
		}
		return sttprovider.NormalizedAudio{}, fmt.Errorf("%w: audio decode failed: %s", sttprovider.ErrInvalidRequest, commandDetail(err, stderr))
	}
	wav, err := pcm16WAV(pcm, format.SampleRate, format.Channels)
	if err != nil {
		return sttprovider.NormalizedAudio{}, err
	}
	return sttprovider.NormalizedAudio{Audio: wav, Converted: true}, nil
}

func pcm16WAV(pcm []byte, sampleRate, channels int) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, fmt.Errorf("%w: decoded audio contains no samples", sttprovider.ErrInvalidRequest)
	}
	blockAlign := channels * 2
	if len(pcm)%blockAlign != 0 {
		return nil, fmt.Errorf("%w: decoded PCM is not sample-aligned", sttprovider.ErrUnavailable)
	}
	if len(pcm) > int(^uint32(0))-36 {
		return nil, fmt.Errorf("%w: normalized audio is too large for WAV", sttprovider.ErrInvalidRequest)
	}
	const bitsPerSample = 16
	byteRate := sampleRate * blockAlign
	wav := make([]byte, 44+len(pcm))
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(wav[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(wav[34:36], bitsPerSample)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return wav, nil
}

func matchesWAV(audio []byte, required sttprovider.AudioFormat) bool {
	if !strings.EqualFold(required.Container, "wav") || !strings.EqualFold(required.Codec, "pcm_s16le") || len(audio) < 12 ||
		string(audio[:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		return false
	}
	if int(binary.LittleEndian.Uint32(audio[4:8])) != len(audio)-8 {
		return false
	}
	formatMatches := false
	dataFound := false
	for offset := 12; offset+8 <= len(audio); {
		size := int(binary.LittleEndian.Uint32(audio[offset+4 : offset+8]))
		dataStart := offset + 8
		dataEnd := dataStart + size
		if size < 0 || dataEnd < dataStart || dataEnd > len(audio) {
			return false
		}
		if string(audio[offset:offset+4]) == "fmt " {
			if size < 16 {
				return false
			}
			format := binary.LittleEndian.Uint16(audio[dataStart : dataStart+2])
			channels := int(binary.LittleEndian.Uint16(audio[dataStart+2 : dataStart+4]))
			sampleRate := int(binary.LittleEndian.Uint32(audio[dataStart+4 : dataStart+8]))
			bitsPerSample := int(binary.LittleEndian.Uint16(audio[dataStart+14 : dataStart+16]))
			formatMatches = format == 1 && channels == required.Channels && sampleRate == required.SampleRate && bitsPerSample == 16
		}
		if string(audio[offset:offset+4]) == "data" && size > 0 {
			dataFound = true
		}
		offset = dataEnd + size%2
	}
	return formatMatches && dataFound
}
