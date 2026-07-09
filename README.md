# audio-server

A standalone audio manager service for TTS generation. Exposes an OpenAI-compatible speech endpoint and routes requests to configured provider backends.

## Providers

| Provider | Type | Status |
|---|---|---|
| `espeak-ng` | local subprocess | shipped |
| `omnivoice` | HTTP sidecar (Python / GPU) | shipped, requires `AUDIO_OMNIVOICE_ADDR` |

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

## List voices

```bash
curl -sS 'http://127.0.0.1:8010/v1/audio/voices?language=en'
```

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
| `AUDIO_REQUEST_TIMEOUT_SECONDS` | `30` | per-request synthesis timeout |
| `AUDIO_MAX_INPUT_CHARS` | `5000` | max request input length |
| `AUDIO_ESPEAK_PATH` | `espeak-ng` | eSpeak binary path |
| `AUDIO_ESPEAK_DEFAULT_VOICE` | `en` | fallback eSpeak voice |
| `AUDIO_FFMPEG_PATH` | `ffmpeg` | ffmpeg binary path |
| `AUDIO_MP3_BITRATE` | `48k` | MP3 bitrate |
| `AUDIO_DEFAULT_PROVIDER` | `espeak-ng` | provider used for `auto`, `tts-1`, and blank model |
| `AUDIO_OMNIVOICE_ADDR` | _(empty)_ | OmniVoice sidecar base URL, e.g. `http://127.0.0.1:8020`; provider disabled when blank |

All flags are also available as CLI flags — run `./bin/audio-server -help` for the full list.

## OmniVoice (Greek TTS)

See [`docs/providers.md`](docs/providers.md#omnivoice) for the sidecar provider, and [`tts/omnivoice/README.md`](tts/omnivoice/README.md) for the standalone CLI script.
