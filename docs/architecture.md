# Architecture

## Overview

```
cmd/audio/main.go          — binary entry point, wires config + providers + queue
internal/server/           — HTTP server, routing, auth
internal/queue/             — per-provider job queue, worker pools, GPU model lifecycle
internal/provider/         — Provider (+ optional Lifecycle) interface + shared types
internal/provider/espeak/    — espeak-ng subprocess provider (implements Streamer)
internal/provider/omnivoice/ — OmniVoice HTTP sidecar client provider (implements Lifecycle)
internal/provider/kokoro/    — Kokoro TTS HTTP sidecar client provider (implements Lifecycle)
internal/sttprovider/        — Provider interface for speech-to-text (separate from Provider/TTS)
internal/provider/fasterwhisper/ — faster-whisper STT HTTP sidecar client provider
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

`internal/queue.Manager` gives each provider ID its own FIFO queue and a fixed-size worker pool (`AUDIO_MAX_CONCURRENCY` for espeak-ng, `AUDIO_OMNIVOICE_CONCURRENCY` for OmniVoice — default 1, since a single 4GB GPU can't run concurrent diffusion jobs). A worker just pulls the next job off its provider's queue and calls the unchanged `provider.Synthesize`; because it doesn't do anything between back-to-back jobs, a queue with several pending OmniVoice requests never reloads the model between them.

For providers with an expensive resource to manage, `provider.Lifecycle` (`Warm`/`Idle`) lets the manager load the model before the first job after an idle period and unload it after the queue has been empty for `AUDIO_OMNIVOICE_IDLE_UNLOAD_SECONDS`. `espeak-ng` doesn't implement it (nothing to warm); `omnivoice` does (`POST /load` / `POST /unload` on the sidecar). A per-provider mutex in the queue manager guarantees a timer-driven `Idle` and a worker-driven `Warm` never run concurrently, so the model can't be unloaded out from under an about-to-start job.

Jobs are tracked in memory only (map keyed by job ID); finished jobs are swept after ~10 minutes. A server restart loses all queued/in-flight/recently-finished jobs — there's no persistence.

**Behavior note:** a job runs to completion independent of any specific HTTP caller. If `/v1/audio/speech`'s wait times out (504) or the client disconnects, the underlying `Synthesize` call is *not* cancelled — other queued jobs and later polls via `/v1/audio/jobs/{id}` depend on it finishing. `Synthesize` itself is still bounded by `AUDIO_REQUEST_TIMEOUT_SECONDS` (applied as the job's own execution timeout), so a wedged provider can't block its worker forever.

## Streaming

`"stream": true` on `POST /v1/audio/speech` takes a deliberately different path from everything above: it bypasses `internal/queue` entirely. The queue model — poll-able jobs, detached execution, GPU lifecycle — exists for async multi-consumer work; a stream is none of those things, it's one live HTTP connection with exactly one consumer. So `server.streamSpeech` calls `provider.Streamer.SynthesizeStream` directly from the request-handling goroutine, gated by its own small per-provider semaphore (`Config.StreamWorkers`, sized the same as the provider's queue workers so total concurrent espeak-ng subprocesses stays bounded). Unlike queued jobs, a stream's context *is* `r.Context()` bounded by `AUDIO_REQUEST_TIMEOUT_SECONDS` — disconnecting the client or hitting the timeout genuinely cancels synthesis, which is correct here since nothing else is waiting on the output.

Only `espeak-ng` implements `provider.Streamer` today (see `docs/providers.md`); `omnivoice` doesn't, so `stream: true` against it silently falls back to the buffered path.

## Speech-to-text

`POST /v1/audio/transcriptions` is a third, independent request path — it doesn't touch `internal/queue`, `provider.Provider`, or any of the TTS machinery above. STT uses its own interface, `sttprovider.Provider` (`internal/sttprovider`), and `server.transcriptions` calls `Transcribe` directly and synchronously, bounded by `AUDIO_REQUEST_TIMEOUT_SECONDS`. This is a deliberate scope choice, not an oversight: the queue exists to solve GPU lifecycle + "see my position in line" for slow generation, and today's only STT provider (faster-whisper) is a quick single round trip that doesn't need that. The faster-whisper sidecar manages its own idle-unload timer internally instead (see `docs/providers.md`) as a lighter-weight stand-in for the same VRAM concern.

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
