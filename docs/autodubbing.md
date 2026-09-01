# Autodubbing analysis

The analysis API turns an audio or video asset into caption-ready speaker
segments. It runs as an asynchronous job and accepts any input FFmpeg can
decode.

## Setup

On Debian/Ubuntu, the project setup script installs native dependencies,
builds audio.cpp, transcribe.cpp, and the official WeSpeaker C++ runtime, then
downloads the configured models:

```bash
./tmp/scripts/setup-autodubbing.sh
make run
```

## Analyze an asset

```bash
RESPONSE=$(curl -sS http://127.0.0.1:8010/v1/audio/analysis-jobs \
  -F file=@episode.mkv \
  -F language=en \
  -F transcribe=true \
  -F include_speaker_embeddings=true \
  -F separate_dialogue=true)

ID=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' \
  <<<"$RESPONSE")
curl -sS "http://127.0.0.1:8010/v1/audio/analysis-jobs/$ID"
curl -sS "http://127.0.0.1:8010/v1/audio/analysis-jobs/$ID/result"
```

The result includes non-overlapping timeline `segments`; `speaker_ids` has
multiple values and `overlap` is true where people speak simultaneously.
`text` is the optional per-segment transcript. `raw_turns` preserves the
diarizer output.

Speaker IDs are local to one asset. Each speaker can also include an
L2-normalized 256-dimensional WeSpeaker embedding. Compare embeddings with
cosine similarity to cluster the same character across episodes, then let a
human review that mapping before choosing TTS reference clips. Embeddings are
biometric data and should be protected accordingly.

## Model and lifecycle behavior

- BS-RoFormer optionally isolates vocals/dialogue from music and effects.
- Sortformer identifies up to four simultaneous/local speakers.
- The configured buffered STT provider captions each timeline segment.
- WeSpeaker creates speaker embeddings on CPU and remains resident.

The GPU stages acquire the server's shared execution gate one at a time and
release it between stages. Models remain loaded when the configured
`max_vram_mib` budget permits; otherwise the existing LRU lifecycle evicts an
idle model. TTS and STT jobs therefore continue to use the same scheduling and
VRAM rules.

## Current boundaries

BS-RoFormer separates dialogue from background, not actor from actor. True
overlapping-speaker source separation, persistent cross-asset character
profiles, final translated TTS timing, and background remixing remain separate
workflow steps.
