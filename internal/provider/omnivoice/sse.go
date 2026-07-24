package omnivoice

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type SSEEvent struct {
	Type   string
	Base64 string
}

func readSSEEvents(r io.Reader, emit func(SSEEvent) error) error {
	buf := make([]byte, 0, 65536)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		for {
			idx := indexLineEnd(buf)
			if idx < 0 {
				break
			}
			line := string(buf[:idx])
			skip := idx + 1
			if skip < len(buf) && buf[idx] == '\r' && buf[skip] == '\n' {
				skip++
			}
			buf = buf[skip:]

			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimSpace(line[6:])
			if data == "[DONE]" {
				return emit(SSEEvent{Type: "done"})
			}

			var raw struct {
				Type  string `json:"type"`
				Audio string `json:"audio"`
				Data  string `json:"data"`
			}
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				continue
			}
			switch raw.Type {
			case "speech.audio.delta":
				b64 := raw.Audio
				if b64 == "" {
					b64 = raw.Data
				}
				if err := emit(SSEEvent{Type: raw.Type, Base64: b64}); err != nil {
					return err
				}
			case "speech.audio.done":
				return emit(SSEEvent{Type: raw.Type})
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("sse read: %w", err)
		}
	}
}

func indexLineEnd(b []byte) int {
	for i, c := range b {
		if c == '\n' || c == '\r' {
			return i
		}
	}
	return -1
}

func DecodeBase64Chunk(data string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(data)
}
