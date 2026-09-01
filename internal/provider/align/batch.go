package align

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const maxBatchItems = 1000

// BatchResult is keyed by the caller-provided item IDs so clients can map MFA's
// per-file output back to their own sentence records without relying on order.
type BatchResult struct {
	Items []BatchResultItem `json:"items"`
}

type BatchResultItem struct {
	ID        string        `json:"id"`
	Alignment WordAlignment `json:"alignment"`
}

type WordAlignment struct {
	Words []WordTiming `json:"words"`
}

type WordTiming struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// AlignBatch creates one MFA corpus and invokes the CLI exactly once. The
// single-item endpoint intentionally remains available for interactive use,
// while callers preparing a story avoid paying Python/Kaldi/model startup for
// every sentence.
func (p *Provider) AlignBatch(ctx context.Context, audio [][]byte, transcripts, ids []string, language string) (any, error) {
	if len(audio) == 0 || len(audio) != len(transcripts) || len(audio) != len(ids) {
		return nil, errors.New("align batch: audio, transcripts, and ids must have the same non-zero length")
	}
	if len(audio) > maxBatchItems {
		return nil, fmt.Errorf("align batch: too many items: %d", len(audio))
	}
	model, ok := p.langs[normalizeLang(language)]
	if !ok {
		model, ok = p.langs["en"]
		if !ok {
			return nil, os.ErrNotExist
		}
	}

	tmpDir, err := os.MkdirTemp("", "mfa-align-batch-")
	if err != nil {
		return nil, fmt.Errorf("align batch: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	corpusDir := filepath.Join(tmpDir, "corpus")
	outputDir := filepath.Join(tmpDir, "output")
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		return nil, fmt.Errorf("align batch: create corpus: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("align batch: create output: %w", err)
	}

	idsByBase := make(map[string]string, len(ids))
	seenIDs := make(map[string]struct{}, len(ids))
	for i := range audio {
		if len(audio[i]) == 0 || strings.TrimSpace(transcripts[i]) == "" || strings.TrimSpace(ids[i]) == "" {
			return nil, fmt.Errorf("align batch: item %d has empty audio, transcript, or id", i)
		}
		if _, exists := seenIDs[ids[i]]; exists {
			return nil, fmt.Errorf("align batch: duplicate id %q", ids[i])
		}
		seenIDs[ids[i]] = struct{}{}
		base := fmt.Sprintf("item_%06d", i)
		idsByBase[base] = ids[i]
		wavPath := filepath.Join(corpusDir, base+".wav")
		if err := convertBatchAudio(audio[i], wavPath); err != nil {
			return nil, fmt.Errorf("align batch: convert item %q: %w", ids[i], err)
		}
		if err := os.WriteFile(filepath.Join(corpusDir, base+".lab"), []byte(transcripts[i]), 0o644); err != nil {
			return nil, fmt.Errorf("align batch: write transcript %q: %w", ids[i], err)
		}
	}

	g2pPath := model.G2P
	if g2pPath != "" && !filepath.IsAbs(g2pPath) {
		cwd, _ := os.Getwd()
		g2pPath = filepath.Join(cwd, g2pPath)
	}
	mfaBin := filepath.Join(p.mfa.MFAEnv, "bin", "mfa")
	args := []string{
		"align", corpusDir, model.Dictionary, model.Acoustic, outputDir,
		"--output_format", "json",
		"--single_speaker",
		"--use_mp",
		"--clean",
		fmt.Sprintf("--temporary_directory=%s", filepath.Join(tmpDir, "mfa-tmp")),
	}
	if g2pPath != "" {
		args = append(args, "--g2p_model_path", g2pPath)
	}
	cmd := exec.CommandContext(ctx, mfaBin, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("MFA_ROOT_DIR=%s", p.mfa.WorkDir),
		fmt.Sprintf("PATH=%s/bin:%s", p.mfa.MFAEnv, os.Getenv("PATH")),
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("align batch: mfa failed: %w", err)
	}

	alignments := make(map[string]WordAlignment, len(ids))
	err = filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		id, ok := idsByBase[strings.TrimSuffix(entry.Name(), ".json")]
		if !ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		alignment, err := parseBatchMFAJSON(data)
		if err != nil {
			return fmt.Errorf("parse %q: %w", id, err)
		}
		alignments[id] = alignment
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("align batch: read output: %w", err)
	}

	result := BatchResult{Items: make([]BatchResultItem, 0, len(ids))}
	for _, id := range ids {
		alignment, ok := alignments[id]
		if !ok {
			return nil, fmt.Errorf("align batch: no alignment output for %q", id)
		}
		result.Items = append(result.Items, BatchResultItem{ID: id, Alignment: alignment})
	}
	return result, nil
}

func convertBatchAudio(audio []byte, outPath string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", "pipe:0", "-ar", "16000", "-ac", "1", outPath)
	cmd.Stdin = bytes.NewReader(audio)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func parseBatchMFAJSON(data []byte) (WordAlignment, error) {
	var raw struct {
		Tiers map[string]struct {
			Entries [][3]any `json:"entries"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return WordAlignment{}, err
	}
	words, ok := raw.Tiers["words"]
	if !ok {
		return WordAlignment{}, errors.New("words tier is missing")
	}
	result := WordAlignment{Words: make([]WordTiming, 0, len(words.Entries))}
	for _, entry := range words.Entries {
		start, startOK := entry[0].(float64)
		end, endOK := entry[1].(float64)
		text, textOK := entry[2].(string)
		if !startOK || !endOK || !textOK {
			return WordAlignment{}, errors.New("invalid words tier entry")
		}
		result.Words = append(result.Words, WordTiming{Text: text, Start: start, End: end})
	}
	return result, nil
}
