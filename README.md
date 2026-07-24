# audio-server

A standalone audio manager service for TTS and STT. Exposes OpenAI-compatible speech and transcription endpoints and routes requests to configured provider backends.

## Providers

| Provider | Type | Status |
|---|---|---|
| `espeak-ng` | local subprocess (TTS) | shipped |
| `omnivoice` | HTTP sidecar (TTS, Python / GPU) | shipped, requires `AUDIO_OMNIVOICE_ADDR` |
| `kokoro` | HTTP sidecar (TTS, Python / GPU or CPU) | shipped, requires `AUDIO_KOKORO_ADDR` |
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

Requires `AUDIO_FASTERWHISPER_ADDR` to be set — see [`docs/providers.md`](docs/providers.md#speech-to-text-providers) and [`docs/api.md`](docs/api.md).

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
| `AUDIO_ADDR` | `127.0.0.1:8010` | listen address |
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
| `AUDIO_AUDIOCPP_IDLE_UNLOAD` | `600` | seconds an empty OmniVoice/Supertonic queue waits before the native sidecar is unloaded |
| `AUDIO_RESOURCE_SWITCH_DELAY_SECONDS` | `1` | quiet seconds after one audio.cpp provider finishes before another shared-GPU provider starts |
| `AUDIO_KOKORO_ADDR` | _(empty)_ | Kokoro sidecar base URL, e.g. `http://127.0.0.1:8021`; provider disabled when blank |
| `AUDIO_KOKORO_CONCURRENCY` | `1` | concurrent Kokoro workers |
| `AUDIO_KOKORO_IDLE_UNLOAD_SECONDS` | `30` | seconds an empty Kokoro queue waits before the model is unloaded |
| `AUDIO_FASTERWHISPER_ADDR` | _(empty)_ | faster-whisper sidecar base URL, e.g. `http://127.0.0.1:8030`; STT disabled when blank |

All flags are also available as CLI flags — run `./bin/audio-server -help` for the full list.

## OmniVoice (Greek TTS)

See [`docs/providers.md`](docs/providers.md#omnivoice) for the sidecar provider, and [`tts/omnivoice/README.md`](tts/omnivoice/README.md) for the standalone CLI script.

## Kokoro (multi-voice TTS)

Used in place of Piper (issue #4), which doesn't run on the target hardware. See [`docs/providers.md`](docs/providers.md#kokoro) for the sidecar provider, and [`tts/kokoro/README.md`](tts/kokoro/README.md) for setup.

## faster-whisper (STT)

See [`docs/providers.md`](docs/providers.md#faster-whisper) for the sidecar provider and why STT doesn't go through the same job queue as TTS.
