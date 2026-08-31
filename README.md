# audio-server

A standalone audio manager service for TTS and STT. Exposes OpenAI-compatible speech and transcription endpoints and routes requests to configured provider backends.

## Providers

| Provider | Type | Status |
|---|---|---|
| `espeak-ng` | local subprocess (TTS) | shipped |
| `omnivoice` | HTTP sidecar (TTS, Python / GPU) | shipped, requires `AUDIO_OMNIVOICE_ADDR` |
| `kokoro` | HTTP sidecar (TTS, Python / GPU or CPU) | shipped, requires `AUDIO_KOKORO_ADDR` |
| `parakeet` | audio.cpp sidecar (STT, native CUDA/CPU) | shipped, configured as the default STT provider |
| `nemotron` | audio.cpp sidecar (streaming-capable STT, native CUDA/CPU) | shipped |
| `faster-whisper` | HTTP sidecar (STT, Python / GPU or CPU) | shipped, requires `AUDIO_FASTERWHISPER_ADDR` |

## Install runtime tools

```bash
# Debian/Ubuntu
sudo apt-get install espeak-ng ffmpeg

# macOS
brew install espeak-ng ffmpeg
```

## Run

```bash
go run ./cmd/audio
```

Open the self-contained interactive API reference at
[`http://127.0.0.1:8010/docs`](http://127.0.0.1:8010/docs), or consume the
OpenAPI 3.1 contract from `http://127.0.0.1:8010/openapi.json`.

## Build

```bash
go build -o bin/audio-server ./cmd/audio
```

## Generate speech

```bash
curl -sS http://127.0.0.1:8010/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"model":"tts-1","input":"hello","voice":"en-us","response_format":"mp3"}' \
  --output /tmp/out.mp3
```

## Generate speech asynchronously (queue + poll)

```bash
JOB=$(curl -sS http://127.0.0.1:8010/v1/audio/jobs \
  -d '{"input":"hello","voice":"en-us","response_format":"wav"}')
ID=$(python3 -c "import sys,json;print(json.load(sys.stdin)['id'])" <<< "$JOB")

curl -sS http://127.0.0.1:8010/v1/audio/jobs/$ID          # status + queue_position
curl -sS http://127.0.0.1:8010/v1/audio/jobs/$ID/audio -o out.wav  # once status is "succeeded"
```

See [`docs/api.md`](docs/api.md) for the full job API — this is the one to use if you want visibility into queue position instead of just blocking on `/v1/audio/speech`.

## Stream speech

```bash
curl -sS --no-buffer http://127.0.0.1:8010/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"input":"a longer piece of text","voice":"en-us","response_format":"mp3","stream":true}' \
  --output out.mp3
```

Audio is flushed to the response as it's produced instead of buffered first. Only `espeak-ng` supports this today (`stream: true` against `omnivoice` just falls back to the buffered response) — see [`docs/api.md`](docs/api.md#streaming-stream-true).

## Transcribe speech (STT)

```bash
curl -sS http://127.0.0.1:8010/v1/audio/transcriptions \
  -F file=@speech.wav \
  -F language=en
```

The checked-in configuration uses Parakeet-TDT by default. Set `model=nemotron`
or `model=faster-whisper` to select another configured STT provider.

The web UI also supports a live microphone mode. Open
`http://127.0.0.1:8010`, click **Start live mic**, and keep speaking. The
browser captures mono PCM continuously and refreshes the cumulative Parakeet
transcript about every three seconds; pressing **Stop live mic** sends one
final snapshot.

## List voices

```bash
curl -sS 'http://127.0.0.1:8010/v1/audio/voices?language=en'
```

List all provider languages and canonical voices:

```bash
curl -sS http://127.0.0.1:8010/v1/audio/capabilities
```

Both `language` and `voice` default to `auto` when omitted. `auto` is a
provider-defined automatic/default selection; use the capabilities endpoint
and the language-filtered voices endpoint when a concrete choice is needed.

## Health check

```bash
curl -sS http://127.0.0.1:8010/healthz
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `AUDIO_ADDR` | `0.0.0.0:8010` in `config.yml` | listen address |
| `AUDIO_API_KEY` | _(empty)_ | optional bearer token |
| `AUDIO_MAX_CONCURRENCY` | `2` | max concurrent synthesis processes |
| `AUDIO_REQUEST_TIMEOUT_SECONDS` | `0` | per-request synthesis timeout (`0` disables the timeout) |
| `AUDIO_MAX_INPUT_CHARS` | `5000` | max request input length |
| `AUDIO_ESPEAK_PATH` | `espeak-ng` | eSpeak binary path |
| `AUDIO_ESPEAK_DEFAULT_VOICE` | `en` | fallback eSpeak voice |
| `AUDIO_FFMPEG_PATH` | `ffmpeg` | ffmpeg binary path |
| `AUDIO_MP3_BITRATE` | `48k` | MP3 bitrate |
| `AUDIO_DEFAULT_PROVIDER` | `espeak-ng` | provider used for `auto`, `tts-1`, and blank model |
| `AUDIO_OMNIVOICE_ADDR` | _(empty)_ | OmniVoice sidecar base URL, e.g. `http://127.0.0.1:8020`; provider disabled when blank |
| `AUDIO_OMNIVOICE_CONCURRENCY` | `1` | concurrent OmniVoice workers — keep at 1 on a single GPU with limited VRAM |
| `AUDIO_AUDIOCPP_IDLE_UNLOAD` | `0` | optional idle unload timeout; `0` keeps models resident until VRAM-budget eviction |
| `AUDIO_MAX_VRAM_MIB` | `auto` | model-residency budget; `auto` uses total VRAM reported by `nvidia-smi` |
| `AUDIO_DEFAULT_STT_PROVIDER` | `parakeet` in `config.yml` | provider used for `auto`, `whisper-1`, and a blank transcription model |
| `AUDIO_PARAKEET_ADDR` | `http://127.0.0.1:8026` in `config.yml` | Parakeet-TDT audio.cpp sidecar base URL |
| `AUDIO_NEMOTRON_ADDR` | `http://127.0.0.1:8024` in `config.yml` | Nemotron audio.cpp sidecar base URL |
| `AUDIO_KOKORO_ADDR` | _(empty)_ | Kokoro sidecar base URL, e.g. `http://127.0.0.1:8021`; provider disabled when blank |
| `AUDIO_KOKORO_CONCURRENCY` | `1` | concurrent Kokoro workers |
| `AUDIO_KOKORO_IDLE_UNLOAD_SECONDS` | `30` | seconds an empty Kokoro queue waits before the model is unloaded |
| `AUDIO_FASTERWHISPER_ADDR` | `http://127.0.0.1:8030` in `config.yml` | lifecycle-managed faster-whisper sidecar URL |
| `AUDIO_FASTERWHISPER_PYTHON` | project pyenv in `config.yml` | Python executable containing faster-whisper and CTranslate2 |
| `AUDIO_FASTERWHISPER_MODEL_SIZE` | `large-v3` in `config.yml` | model downloaded and loaded on the first faster-whisper request |
| `AUDIO_FASTERWHISPER_DEVICE` | `cuda` in `config.yml` | inference device |
| `AUDIO_FASTERWHISPER_COMPUTE_TYPE` | `int8` in `config.yml` | quantization required for the 4 GB GPU |

All flags are also available as CLI flags — run `./bin/audio-server -help` for the full list.

## OmniVoice (Greek TTS)

See [`docs/providers.md`](docs/providers.md#omnivoice) for the sidecar provider, and [`tts/omnivoice/README.md`](tts/omnivoice/README.md) for the standalone CLI script.

## Kokoro (multi-voice TTS)

Used in place of Piper (issue #4), which doesn't run on the target hardware. See [`docs/providers.md`](docs/providers.md#kokoro) for the sidecar provider, and [`tts/kokoro/README.md`](tts/kokoro/README.md) for setup.

## faster-whisper (STT)

Faster-whisper is started on demand by the shared GPU lifecycle manager. It
remains resident with other models when their configured estimates fit the
VRAM budget, or triggers idle LRU eviction when they do not. See
[`docs/providers.md`](docs/providers.md#faster-whisper).

## Parakeet-TDT (primary STT)

Parakeet runs through the native `audio.cpp` lifecycle manager and is started
on the first transcription request. Its server configuration is
[`audiocpp-configs/parakeet.json`](audiocpp-configs/parakeet.json).

## Forced Alignment (MFA)

Call MFA's `align` via Go subprocess. Micromamba environment at `mfa/env/`,
pretrained models at `mfa/pretrained_models/`.

```bash
curl -sS http://127.0.0.1:8010/v1/audio/alignments \
  -F file=@speech.wav \
  -F transcript="the text that was spoken" \
  -F language=el
```

Language codes map to MFA model names via `mfa/models.yaml`.

> **TODO:** Replace the MFA subprocess call with direct Kaldi CGo wiring
> (`gmm-align-compiled` + `compile-train-graphs`).  The Kaldi C++ libraries
> already ship inside the micromamba environment; the CGo path would remove
> the Python orchestration layer while keeping the same GMM-HMM models.
