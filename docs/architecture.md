# Architecture

## Overview

```
cmd/audio/main.go          — binary entry point, wires config + providers + queue
internal/server/           — HTTP server, routing, auth
internal/queue/             — per-provider queues, worker pools, shared-resource lifecycle
internal/provider/         — Provider (+ optional Lifecycle) interface + shared types
internal/provider/espeak/    — espeak-ng subprocess provider (implements Streamer)
internal/provider/omnivoice/ — OmniVoice HTTP sidecar client provider (implements Lifecycle)
internal/provider/kokoro/    — Kokoro TTS HTTP sidecar client provider (implements Lifecycle)
internal/sttprovider/        — Provider interface for speech-to-text (separate from Provider/TTS)
internal/provider/fasterwhisper/ — faster-whisper STT HTTP sidecar client provider
internal/provider/parakeet/    — Parakeet-TDT audio.cpp STT client provider
internal/encode/             — ffmpeg WAV→{MP3,OGG,Opus,FLAC} encoder
internal/run/                — thin subprocess abstraction (testable Command type)
tts/espeak-ng/                — espeak-ng setup notes (no code, system binary only)
tts/omnivoice/                — OmniVoice HTTP sidecar (server.py) + standalone CLI script
tts/kokoro/                   — Kokoro TTS HTTP sidecar (server.py)
stt/fasterwhisper/            — faster-whisper HTTP sidecar (server.py)
```

## Request flow

```
POST /v1/audio/speech                      POST /v1/audio/jobs
  → auth middleware                          → auth middleware
  → decode + validate request                → decode + validate request
  → server.providerFor(model, language)       → server.providerFor(model, language)
  → queue.Submit(providerID, req)             → queue.Submit(providerID, req) — returns immediately
  → queue.Wait(ctx, jobID)                        │
       bounded by AUDIO_REQUEST_TIMEOUT_SECONDS    ▼
  → write audio bytes + X-TTS-* headers       GET /v1/audio/jobs/{id}        → poll status + queue_position
                                               GET /v1/audio/jobs/{id}/audio  → fetch result once succeeded
```

Both entry points submit to the same `queue.Manager`; `/v1/audio/speech` just also blocks on `Wait` so simple/fast callers (espeak-ng) don't need to poll. See `docs/api.md` for the job endpoint contracts.

## Queue and GPU model lifecycle

`internal/queue.Manager` gives each provider ID its own FIFO queue and a fixed-size worker pool (`AUDIO_MAX_CONCURRENCY` for espeak-ng, `AUDIO_OMNIVOICE_CONCURRENCY` for OmniVoice). GPU-backed TTS jobs additionally acquire the shared lifecycle manager's execution lease. STT remains synchronous rather than joining the TTS queue, but acquires the same lease, so only one CUDA inference runs at a time across both directions.

The lifecycle manager keeps multiple model sidecars resident when their configured `model_vram_mib` estimates fit within `max_vram_mib`. The default `auto` budget uses total VRAM reported by `nvidia-smi`. When a requested model would exceed the budget, idle models are stopped least-recently-used first. A model holding an execution lease is never evicted. This avoids model switching on roomy GPUs while retaining safe dynamic eviction on smaller GPUs.

Jobs are tracked in memory only (map keyed by job ID); finished jobs are swept after ~10 minutes. A server restart loses all queued/in-flight/recently-finished jobs — there's no persistence.

**Behavior note:** a job runs to completion independent of any specific HTTP caller. If `/v1/audio/speech`'s wait times out (504) or the client disconnects, the underlying `Synthesize` call is *not* cancelled — other queued jobs and later polls via `/v1/audio/jobs/{id}` depend on it finishing. `Synthesize` itself is still bounded by `AUDIO_REQUEST_TIMEOUT_SECONDS` (applied as the job's own execution timeout), so a wedged provider can't block its worker forever.

## Streaming

`"stream": true` uses direct streaming for non-lifecycle providers, gated by the per-provider semaphore (`Config.StreamWorkers`). Lifecycle-managed streamers are submitted as queue jobs: the queue performs warm-up and shared-resource scheduling, then the worker invokes `SynthesizeStream` and writes chunks to the waiting HTTP response. The provider stream is tied to the live HTTP connection and is cancelled when it disconnects.

Only `espeak-ng` implements `provider.Streamer` today (see `docs/providers.md`); `omnivoice` doesn't, so `stream: true` against it silently falls back to the buffered path.

## Speech-to-text

`POST /v1/audio/transcriptions` doesn't use the TTS job queue or `provider.Provider`. STT uses its own `sttprovider.Provider` interface, plus the optional `StreamingProvider` interface for cumulative partial results. Normal requests call `Transcribe`; `stream=true` relays provider partials as SSE. Both paths use the shared GPU execution/residency manager. Faster-whisper is registered as an arbitrary Python command while the native providers use audio.cpp configuration files.

## Testability

`internal/run` defines a `Command` function type that wraps `os/exec`. Tests replace it with a stub, so provider tests never spawn real processes.

The `espeak.Encoder` interface lets tests swap out ffmpeg without the binary present.

## Provider model routing

The `model` field in speech requests is used to select a provider:

| `model` value | Resolves to |
|---|---|
| `""`, `auto`, `tts-1`, `tts-1-hd`, `gpt-4o-mini-tts` | default provider, unless `language` matches a language-specific provider (e.g. `el` → `omnivoice`) |
| provider ID (e.g. `espeak-ng`, `omnivoice`) | that provider directly |
| anything else | 400 unknown model |

This keeps the API OpenAI-compatible while allowing direct provider targeting.
