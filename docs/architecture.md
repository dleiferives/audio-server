# Architecture

## Overview

```
cmd/audio/main.go          — binary entry point, wires config + providers
internal/server/           — HTTP server, routing, auth, concurrency semaphore
internal/provider/         — Provider interface + shared types
internal/provider/espeak/  — espeak-ng subprocess provider
internal/encode/           — ffmpeg WAV→MP3 encoder
internal/run/              — thin subprocess abstraction (testable Command type)
tts/el/                    — standalone Python Greek TTS script (OmniVoice)
```

## Request flow

```
POST /v1/audio/speech
  → auth middleware (bearer token / X-API-Key)
  → semaphore acquire (AUDIO_MAX_CONCURRENCY slots)
  → context timeout (AUDIO_REQUEST_TIMEOUT_SECONDS)
  → server.providerFor(model) — resolves OpenAI aliases to provider ID
  → provider.Synthesize(ctx, req)
       espeak-ng: shell out → WAV bytes → ffmpeg encode → MP3 bytes
  → write audio bytes + X-TTS-* response headers
```

## Concurrency model

A channel-based semaphore in `server.Server` limits simultaneous synthesis processes. Requests that cannot acquire a slot wait on the request context and return 504 on timeout.

## Testability

`internal/run` defines a `Command` function type that wraps `os/exec`. Tests replace it with a stub, so provider tests never spawn real processes.

The `espeak.Encoder` interface lets tests swap out ffmpeg without the binary present.

## Provider model routing

The `model` field in speech requests is used to select a provider:

| `model` value | Resolves to |
|---|---|
| `""`, `auto`, `tts-1`, `tts-1-hd`, `gpt-4o-mini-tts` | default provider |
| provider ID (e.g. `espeak-ng`) | that provider directly |
| anything else | 400 unknown model |

This keeps the API OpenAI-compatible while allowing direct provider targeting.
