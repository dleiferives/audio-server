// Package pcmwav reads and writes the canonical mono PCM16 WAV used by analysis providers.
package pcmwav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type Audio struct {
	SampleRate int
	Samples    []int16
}

func Parse(data []byte) (Audio, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return Audio{}, errors.New("not a RIFF/WAVE file")
	}
	var format, channels, bits uint16
	var sampleRate uint32
	var pcm []byte
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + size
		if size < 0 || end < start || end > len(data) {
			return Audio{}, errors.New("invalid WAV chunk size")
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return Audio{}, errors.New("invalid WAV format chunk")
			}
			format = binary.LittleEndian.Uint16(data[start : start+2])
			channels = binary.LittleEndian.Uint16(data[start+2 : start+4])
			sampleRate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			bits = binary.LittleEndian.Uint16(data[start+14 : start+16])
		case "data":
			pcm = data[start:end]
		}
		offset = end + size%2
	}
	if format != 1 || channels != 1 || bits != 16 || sampleRate == 0 {
		return Audio{}, fmt.Errorf("expected mono PCM16 WAV, got format=%d channels=%d bits=%d rate=%d", format, channels, bits, sampleRate)
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return Audio{}, errors.New("WAV contains no complete PCM16 samples")
	}
	samples := make([]int16, len(pcm)/2)
	if err := binary.Read(bytes.NewReader(pcm), binary.LittleEndian, samples); err != nil {
		return Audio{}, err
	}
	return Audio{SampleRate: int(sampleRate), Samples: samples}, nil
}

func Encode(audio Audio) ([]byte, error) {
	if audio.SampleRate <= 0 || len(audio.Samples) == 0 {
		return nil, errors.New("sample rate and samples are required")
	}
	dataSize := len(audio.Samples) * 2
	if dataSize > int(^uint32(0))-36 {
		return nil, errors.New("WAV is too large")
	}
	var out bytes.Buffer
	out.Grow(44 + dataSize)
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(36+dataSize))
	out.WriteString("WAVEfmt ")
	_ = binary.Write(&out, binary.LittleEndian, uint32(16))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint32(audio.SampleRate))
	_ = binary.Write(&out, binary.LittleEndian, uint32(audio.SampleRate*2))
	_ = binary.Write(&out, binary.LittleEndian, uint16(2))
	_ = binary.Write(&out, binary.LittleEndian, uint16(16))
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(dataSize))
	if err := binary.Write(&out, binary.LittleEndian, audio.Samples); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
