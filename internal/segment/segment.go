// Package segment splits synthesis input into chunks small enough for a TTS
// model to hold in memory at once.
//
// Peak VRAM grows with input length, so a long request has to be cut somewhere.
// Sentence boundaries are the natural place: they are where prosody resets, so
// a boundary in the wrong place is audible. Finding them is delegated to
// tts/segment/segment.py; packing sentences into chunks happens here.
package segment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Segmenter reports where sentences end in text.
type Segmenter interface {
	Segment(ctx context.Context, text, language string) ([]string, error)
}

// Python runs the pysbd helper as a short-lived subprocess. Startup is ~40ms,
// which is noise next to the synthesis it feeds.
type Python struct {
	Bin    string // python interpreter
	Script string // path to tts/segment/segment.py
}

type request struct {
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
}

type response struct {
	Sentences []string `json:"sentences"`
	Error     string   `json:"error"`
}

// Segment returns text split into sentences. An empty or whitespace-only input
// yields no sentences rather than an error.
func (p Python) Segment(ctx context.Context, text, language string) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	payload, err := json.Marshal(request{Text: text, Language: language})
	if err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}

	cmd := exec.CommandContext(ctx, p.Bin, p.Script)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var resp response
	// The helper reports its own failures as JSON on stdout, so decode before
	// trusting the exit status.
	if decErr := json.Unmarshal(stdout.Bytes(), &resp); decErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("segment: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("segment: unreadable response: %w", decErr)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("segment: %s", resp.Error)
	}
	if runErr != nil {
		return nil, fmt.Errorf("segment: %v", runErr)
	}
	return resp.Sentences, nil
}

// Pack groups sentences into chunks of at most maxWords, preferring to close a
// chunk once it reaches targetWords so chunks land on sentence boundaries.
//
// A sentence longer than maxWords on its own cannot be honoured at a sentence
// boundary, so it is split on word boundaries instead. That split is audible,
// which is the cost of a hard cap.
func Pack(sentences []string, targetWords, maxWords int) []string {
	if maxWords <= 0 {
		return nil
	}
	if targetWords <= 0 || targetWords > maxWords {
		targetWords = maxWords
	}

	var chunks []string
	var cur []string
	curWords := 0

	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, strings.Join(cur, " "))
			cur = nil
			curWords = 0
		}
	}

	for _, sentence := range sentences {
		words := strings.Fields(sentence)
		if len(words) == 0 {
			continue
		}
		if len(words) > maxWords {
			// Oversized sentence: emit what is pending, then cut on words.
			flush()
			for start := 0; start < len(words); start += maxWords {
				end := start + maxWords
				if end > len(words) {
					end = len(words)
				}
				chunks = append(chunks, strings.Join(words[start:end], " "))
			}
			continue
		}
		if curWords > 0 && curWords+len(words) > targetWords {
			flush()
		}
		cur = append(cur, sentence)
		curWords += len(words)
	}
	flush()
	return chunks
}

// WordCount reports the number of whitespace-separated words in text.
func WordCount(text string) int {
	return len(strings.Fields(text))
}
