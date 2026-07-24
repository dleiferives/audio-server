# Providers

A provider implements the `provider.Provider` interface (`internal/provider/provider.go`):

```go
type Provider interface {
    ID() string
    Health(ctx context.Context) error
    Voices(ctx context.Context, language string) ([]Voice, error)
    Synthesize(ctx context.Context, req SpeechRequest) (SpeechResult, error)
}
```

Providers are registered in `cmd/audio/main.go` and routed by the `model` field in speech requests. OpenAI model aliases (`tts-1`, `tts-1-hd`, `gpt-4o-mini-tts`, `auto`) all resolve to the configured default provider.

Every buffered request runs through `internal/queue`, which gives each provider its own FIFO queue and worker pool (see `docs/architecture.md`). Providers that share an exclusive resource are coordinated by the same manager. `Synthesize` itself is unchanged by this — a provider still just does one request in, one result out.

### Optional: `provider.Lifecycle`

```go
type Lifecycle interface {
    Warm(ctx context.Context) error
    Idle(ctx context.Context) error
}
```

Providers with an expensive resource to manage (a GPU-resident model) can implement this. The queue manager calls `Warm` before the first job dispatched to a cold provider, and `Idle` after that provider's queue has been empty for its configured idle-unload delay — so the model loads once and stays loaded across a run of back-to-back jobs, rather than reloading per request. Providers that don't implement it (like `espeak-ng`) are unaffected — the manager just skips the lifecycle calls.

### Optional: `provider.Streamer`

```go
type Streamer interface {
    SynthesizeStream(ctx context.Context, req SpeechRequest, onHeader func(StreamMeta), w io.Writer) error
}
```

Providers that can produce audio progressively (rather than only a complete buffer) can implement this to support `"stream": true` on `POST /v1/audio/speech`. Non-lifecycle providers bypass `internal/queue` for this path and use the per-provider `Config.StreamWorkers` limit. Lifecycle-managed streamers are queued first, then stream through the worker after warm-up/resource scheduling. `SynthesizeStream` must call `onHeader` exactly once, before writing any bytes to `w`.

## espeak-ng

**ID:** `espeak-ng`  
**Package:** `internal/provider/espeak`  
**Requires:** `espeak-ng` binary, `ffmpeg` binary (for MP3 encoding)

See [`tts/espeak-ng/README.md`](../tts/espeak-ng/README.md) for setup notes.

The espeak provider shells out to `espeak-ng --stdin --stdout` to generate WAV, then pipes the result through `ffmpeg` to encode `mp3`, `ogg`, `opus`, or `flac`. WAV output is returned directly without encoding.

### Voice selection

The audio-server validates an explicit voice against `Voices(ctx, language)`
before enqueueing synthesis. This keeps language eligibility in the provider
instead of in each client. An omitted voice is normalized to `"auto"`; the
provider then chooses its own valid default for the request language. For
eSpeak, that means the language voice when one is supplied, then the configured
`AUDIO_ESPEAK_DEFAULT_VOICE` (default: `en`).

Providers may advertise `auto` as a language. The server uses `language:
"auto"` when the request omits language, and only offers that choice for
providers that declare automatic/default language handling. Supertonic's
adapter maps `auto` to its default `en` tag because the native Supertonic
tokenizer requires a concrete language tag.

### Speed mapping

The `speed` field (OpenAI-style multiplier) is converted to words-per-minute:

```
wpm = round(175 * speed)   clamped to [80, 450]
```

### Encoder interface

The espeak provider accepts any `Encoder` implementation:

```go
type Encoder interface {
    Health(ctx context.Context) error
    Encode(ctx context.Context, wav []byte, format string) ([]byte, string, error)
    EncodeStream(ctx context.Context, wav io.Reader, format string, w io.Writer) (string, error)
}
```

The shipped implementation is `FFmpeg` (`internal/encode/ffmpeg.go`).

### Streaming

`espeak-ng` implements `provider.Streamer`. For `wav`, espeak-ng's stdout is piped directly to the response via `run.ExecStream` — no buffering at all. For any encoded format (`mp3`, `ogg`, `opus`, `flac`), espeak-ng's stdout feeds `ffmpeg`'s stdin through an `io.Pipe` (via `Encoder.EncodeStream`) while `ffmpeg`'s stdout feeds the response, both running concurrently — matching the shape of the buffered path (`Encode`) but without materializing either the WAV or encoded bytes in memory first.

## omnivoice

**ID:** `omnivoice`  
**Package:** `internal/provider/omnivoice`  
**Sidecar:** `tts/omnivoice/server.py`  
**Requires:** Python 3.10+, CUDA-enabled PyTorch, `omnivoice==0.1.5` (sidecar); nothing on the Go side beyond network access to the sidecar

The OmniVoice model (`k2-fsa/OmniVoice`) handles multilingual synthesis via diffusion. It chunks long input internally and reuses the first generated voice for consistency. The audio.cpp sidecar supports pseudo-streaming: it emits SSE `speech.audio.delta` events for generated text chunks. The Go provider decodes those events to raw PCM for `SynthesizeStream`; non-PCM buffered responses are encoded via the shared `encode.FFmpeg` instance.

The Go provider is a thin HTTP client: it calls `GET /health`, `GET /voices`, `POST /synthesize`, and (via `provider.Lifecycle`) `POST /load` / `POST /unload` on the sidecar. Enable it by setting `AUDIO_OMNIVOICE_ADDR` (or `-omnivoice-addr`) to the sidecar's base URL, e.g. `http://127.0.0.1:8020`; the provider is not registered when this is blank, so the main server starts fine without the sidecar running.

Run the sidecar independently:

```bash
python -m pip install torch==2.8.0+cu128 torchaudio==2.8.0+cu128 \
  --extra-index-url https://download.pytorch.org/whl/cu128
python -m pip install omnivoice==0.1.5
./tts/omnivoice/server.py --port 8020
```

### GPU model lifecycle (VRAM budget)

The sidecar doesn't load the model at startup — it loads lazily, on the queue manager's first `Warm` call (or on the first direct `/synthesize` call, defensively). This matters on VRAM-constrained boxes: the model stays resident while OmniVoice jobs keep arriving, and the native sidecar is unloaded after the queue has been empty for `AUDIO_AUDIOCPP_IDLE_UNLOAD` (default 600s). `AUDIO_OMNIVOICE_CONCURRENCY` (default **1**) caps how many OmniVoice jobs run at once — keep this at 1 on a single GPU with limited VRAM, since the sidecar has no concept of splitting VRAM across concurrent generations. Pass `--preload` / `OMNIVOICE_PRELOAD=1` to the sidecar if you'd rather it load eagerly at startup (e.g. for a dedicated GPU box where idle-unload isn't needed).

### Routing

- `"model": "omnivoice"` routes directly to this provider
- `"language": "el"` with no explicit model also routes here (see `languageProviders` in `internal/server/server.go`)

OmniVoice accepts `language: "auto"` as well as concrete language tags. Its
catalog advertises the automatic voice because the underlying model supports
many languages rather than exposing a finite built-in voice list. The audio
server can report and validate that catalog while the sidecar is cold.

### `provider_options`

```json
{ "steps": 16, "seed": 7, "chunk_seconds": 8, "chunk_threshold": 10 }
```

| Field | Type | Default | Description |
|---|---|---|---|
| `steps` | int | 32 | Diffusion steps. Fewer steps = faster, lower quality. Must be ≥ 1. |
| `seed` | int | 42 | Random seed for reproducible generation. |
| `chunk_seconds` | float | 12.0 | Target duration of chunks used for long text. Must be > 0. |
| `chunk_threshold` | float | 18.0 | Estimated duration at which long-text chunking starts. Must be > 0. |

Unknown fields or out-of-range values return `400` with `invalid audio request` (`provider.ErrInvalidRequest`) — validated in `internal/provider/omnivoice`, not the core server.

See [`tts/omnivoice/README.md`](../tts/omnivoice/README.md) for the standalone CLI script (`greek_tts.py`), which the sidecar's model-loading logic is based on.

## kokoro

**ID:** `kokoro`  
**Package:** `internal/provider/kokoro`  
**Sidecar:** `tts/kokoro/server.py`  
**Requires:** Python 3.10+, `torch`, `kokoro`, `soundfile` (sidecar); nothing on the Go side beyond network access to the sidecar

[Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M) is a small (~82M parameter), fast, multi-voice, multi-language TTS model — this is the provider we use instead of Piper (issue #4), since Piper doesn't run on the target hardware. Unlike OmniVoice, Kokoro is light enough to run on CPU if needed, though GPU is still faster.

The Go provider mirrors OmniVoice's shape exactly: a thin HTTP client calling `GET /health`, `GET /voices`, `POST /synthesize`, and (via `provider.Lifecycle`) `POST /load` / `POST /unload`. Enable it with `AUDIO_KOKORO_ADDR` (or `-kokoro-addr`), e.g. `http://127.0.0.1:8021`; disabled when blank. The sidecar only ever produces WAV — non-`wav` formats are encoded via the same shared `encode.FFmpeg` instance as espeak-ng and OmniVoice (`internal/provider/kokoro.Provider.Encoder`). Kokoro doesn't implement `provider.Streamer` either.

Run the sidecar independently — see [`tts/kokoro/README.md`](../tts/kokoro/README.md) for install steps:

```bash
./tts/kokoro/server.py --port 8021
```

### GPU model lifecycle (VRAM budget)

Same pattern as OmniVoice: the sidecar lazily loads a single shared `KModel` (the ~82M-parameter model itself is language-independent) plus a `KPipeline` per requested language (cached, created lazily) on the queue manager's first `Warm` call. Kokoro's static voice catalog remains available to clients while the sidecar is cold.

### Routing

- `"model": "kokoro"` routes directly to this provider — it's not wired into any automatic `language` routing today (unlike OmniVoice's `el` → `omnivoice`), since its languages overlap with espeak-ng's default English handling and we didn't want to silently change existing default behavior.

### Voice selection

Same shape as espeak-ng: `voice` from the request (default `af_heart`, American English female) if set and not `"auto"`, otherwise falls back to the default. `language` (BCP-47, e.g. `en-us`, `en-gb`, `ja`, `zh`) selects which Kokoro `lang_code`/G2P frontend to use; see `LANGUAGE_TO_LANG_CODE` in `tts/kokoro/server.py`.

## Speech-to-text providers

STT is a separate interface, `sttprovider.Provider` (`internal/sttprovider/sttprovider.go`):

```go
type Provider interface {
    ID() string
    Health(ctx context.Context) error
    Transcribe(ctx context.Context, req TranscriptionRequest) (TranscriptionResult, error)
}
```

It's intentionally not `provider.Provider` — the request/result shapes differ (audio bytes + language in, text out), and **STT doesn't route through `internal/queue`** the way TTS does. That queue exists to solve a specific problem: GPU model lifecycle for slow, VRAM-heavy generation where callers benefit from seeing queue position. Transcription requests are typically one quick round trip, so `internal/server.transcriptions` calls `Transcribe` directly and synchronously, bounded by the same `AUDIO_REQUEST_TIMEOUT_SECONDS`. If STT ever needs the same queue treatment (e.g. a much larger Whisper model that's slow enough to want queuing), that's a deliberate follow-up, not something papered over here.

Registered via `server.Config.SttProviders` / `DefaultSttProvider` — entirely optional; `POST /v1/audio/transcriptions` returns `503` if none are configured.

### faster-whisper

**ID:** `faster-whisper`  
**Package:** `internal/provider/fasterwhisper`  
**Sidecar:** `stt/fasterwhisper/server.py`  
**Requires:** Python 3.10+, `faster-whisper`, `torch` (sidecar); nothing on the Go side beyond network access to the sidecar

[faster-whisper](https://github.com/SYSTRAN/faster-whisper) (CTranslate2-based Whisper reimplementation) transcribes audio to text, with automatic language detection if `language` isn't given. The sidecar accepts raw audio bytes directly (no need to specify the format — it decodes via the same machinery Whisper/ffmpeg use internally, so WAV/MP3/etc. all work without pre-conversion).

Enable it with `AUDIO_FASTERWHISPER_ADDR` (or `-faster-whisper-addr`), e.g. `http://127.0.0.1:8030`. Run the sidecar independently:

```bash
pip install faster-whisper torch
./stt/fasterwhisper/server.py --port 8030 --model-size small
```

**Model lifecycle, and why it's different from the TTS sidecars:** since there's no Go-orchestrated queue driving this provider (see above), the sidecar manages its own idle-unload timer internally — it loads the model lazily on first `/transcribe` call and unloads it after `--idle-unload-seconds` (default 60s, resettable via `FASTERWHISPER_IDLE_UNLOAD_SECONDS`) of no requests, resetting the timer on every transcription. `POST /load` / `POST /unload` are still exposed on the sidecar (same shape as the TTS sidecars) so a future queue-based integration could drive it explicitly instead.

## Adding a provider

1. Create a package under `internal/provider/<name>/`
2. Implement `provider.Provider`
3. Instantiate in `cmd/audio/main.go` and pass to `server.Config.Providers`
4. If the provider needs a script, model, or sidecar, put it under `tts/<name>/` (e.g. `tts/omnivoice/`, `tts/espeak-ng/`) or `stt/<name>/` for speech-to-text (e.g. `stt/fasterwhisper/`).
5. If the provider has settings beyond the shared `SpeechRequest` fields (voice/language/speed/format), define a provider-owned `Options` struct and decode it from `SpeechRequest.ProviderOptions` (`json.RawMessage`) with `DisallowUnknownFields`. Don't add provider-specific fields to the shared `SpeechRequest` type — see OmniVoice's `steps`/`seed`/`chunk_seconds`/`chunk_threshold` for the pattern.

The server routes requests by provider ID; the default provider handles all OpenAI model aliases.
