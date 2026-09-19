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

Every buffered request runs through `internal/queue`, which gives each provider its own FIFO queue and worker pool (see `docs/architecture.md`). CUDA providers also acquire the shared GPU execution lease. `Synthesize` itself is unchanged by this — a provider still just does one request in, one result out.

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

#### Auto-segmentation

OmniVoice peak VRAM grows with input length. On a 4 GB card 128 words fit, while
264 words fail trying to allocate a single 2004 MiB buffer — the model is fine,
the request is simply too long to hold at once. Two config keys bound the work
per synthesis:

| Key | Meaning |
|---|---|
| `omnivoice_max_words` | Hard cap on words per chunk. **Unset or `0` disables segmentation**, and input is sent whole. |
| `omnivoice_target_words` | Preferred chunk size; a chunk closes once it reaches this. Defaults to half of `omnivoice_max_words`. Usually 1-3 sentences. |
| `segment_python` / `segment_script` | Interpreter and path for `tts/segment/segment.py`. |

Sentence boundaries come from `pysbd` via `tts/segment/segment.py` (~40ms per
call). `internal/segment.Pack` groups those sentences into chunks, and the
provider hands them to the sidecar newline-separated with
`text_chunk_mode=endline`, which cuts at those newlines and cross-fades between
chunks so the seam is inaudible.

The boundaries have to come from a real segmenter: the C++ `endline` splitter
treats every `.` as a sentence break, so left to itself it cuts `Dr. Smith` in
half. Measured on this GPU with `64`/`32`, a 264-word request holds flat at
1690 MiB and produces 84.6s of audio, where the same request unsegmented
OOMs.

Two limits worth knowing. A single sentence longer than `omnivoice_max_words`
cannot be split at a sentence boundary, so it is cut on word boundaries
instead — audible, but bounded. And `endline` only cuts where a chunk ends in
`.`, `!`, or `?`; for input with no terminal punctuation the codepoint budget
(`max_words * 8`) is the backstop.

Supertonic is deliberately left unsegmented: its memory does not scale the same
way, so long input is passed straight through.

The Go provider is a thin HTTP client: it calls `GET /health`, `GET /voices`, `POST /synthesize`, and (via `provider.Lifecycle`) `POST /load` / `POST /unload` on the sidecar. Enable it by setting `AUDIO_OMNIVOICE_ADDR` (or `-omnivoice-addr`) to the sidecar's base URL, e.g. `http://127.0.0.1:8020`; the provider is not registered when this is blank, so the main server starts fine without the sidecar running.

Run the sidecar independently:

```bash
python -m pip install torch==2.8.0+cu128 torchaudio==2.8.0+cu128 \
  --extra-index-url https://download.pytorch.org/whl/cu128
python -m pip install omnivoice==0.1.5
./tts/omnivoice/server.py --port 8020
```

### GPU model lifecycle (VRAM budget)

The sidecar doesn't load the model at startup — it loads lazily, on the queue manager's first `Warm` call (or on the first direct `/synthesize` call, defensively). The model then remains resident until the VRAM budget requires eviction. `AUDIO_AUDIOCPP_IDLE_UNLOAD` defaults to `0`; set a positive number to additionally unload a model after that many idle seconds. `AUDIO_OMNIVOICE_CONCURRENCY` (default **1**) caps how many OmniVoice jobs run at once.

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

### Per-request speaker and emotion reference

`POST /v1/audio/speech` and `POST /v1/audio/jobs` accept a multipart
`speaker_reference` file plus its required `speaker_reference_text`. For
OmniVoice this is a combined reference: the same clip conditions both speaker
identity and speaking style/emotion. FFmpeg accepts and normalizes common
compressed containers before the native runtime reads the reference. The
temporary normalized WAV is removed after the provider finishes, and no
reusable voice profile is created.

The capabilities endpoint advertises `"reference_modes":["combined"]` for
OmniVoice. Separate speaker and emotion references are intentionally not
emulated; an `emotion_reference` upload returns `400` until a provider with
native separation is integrated.

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

It's intentionally not `provider.Provider` — the request/result shapes differ (audio bytes + language in, text out), and STT doesn't route through `internal/queue` the way TTS does. `internal/server.transcriptions` calls `Transcribe` directly and synchronously, bounded by `AUDIO_REQUEST_TIMEOUT_SECONDS`. CUDA STT still acquires the same lifecycle execution lease as CUDA TTS, so inference is serialized without forcing resident models to unload.

Registered via `server.Config.SttProviders` / `DefaultSttProvider` — entirely optional; `POST /v1/audio/transcriptions` returns `503` if none are configured.

Providers with a strict input format implement
`sttprovider.AudioRequirementsProvider`. Before buffered or streaming dispatch,
the server uses the shared ffmpeg normalizer to decode the upload and produce
the declared sample rate, channels, codec, and container. Already-conforming
PCM WAV input is passed through without invoking ffmpeg. Parakeet, Nemotron,
Qwen3-ASR, Moonshine, and transcribe.cpp currently declare mono 16 kHz PCM WAV; faster-whisper accepts the
original upload directly. Invalid or undecodable input returns `400` before a
provider is started.

### Moonshine (CPU only)

**ID:** `moonshine`

**Package:** `internal/provider/moonshine`

**Runtime:** `stt/moonshine/server.py` on the `moonshine-voice` pip package
(Moonshine v2). Set up with `make setup-moonshine`.

The only STT provider that is CPU-only by construction. The sidecar clears
`CUDA_VISIBLE_DEVICES` before the native library loads, and the provider is
registered with the lifecycle manager at a **zero VRAM cost**, so it is neither
counted against `max_vram_mib` nor evicted to make room for a GPU model. That
also means it is the provider to use when the GPU is busy with other work; see
`config.local.yml` for a profile that runs nothing but this and espeak-ng.

Moonshine v2 genuinely streams — the encoder caches its output and part of the
decoder state and refines the transcript as audio arrives — so it implements
`sttprovider.LiveStreamingProvider` natively rather than replaying buffered
results. `GET /v1/audio/transcriptions/stream?model=moonshine` reports growing
`committed_text` with a revising `tentative_text`.

Moonshine publishes **one model per language** (`ar de en es ja ko tl uk vi zh`),
so the request language selects the model; `moonshine_language` is the default
and switching language swaps the resident model. For English the streaming
architectures are `MEDIUM_STREAMING` (default), `SMALL_STREAMING`, and
`TINY_STREAMING` — there is no `BASE_STREAMING`, and `MEDIUM` measures both
faster and more accurate than `SMALL` on CPU. Unlike Moonshine v1 there is no
64-second input ceiling. See `stt/moonshine/README.md` for the HTTP surface and
measurements.

### transcribe.cpp

**Default ID:** `cohere-transcribe`

**Package:** `internal/provider/transcribecpp`

**Runtime:** pinned `transcribe.cpp` submodule with the native sidecar in `stt/transcribecpp`

The sidecar uses transcribe.cpp's stable C API and keeps its GGUF model loaded
between requests. It is not Cohere-specific: changing `transcribecpp_model` and
`transcribecpp_provider_id` can expose any model family supported by the pinned
runtime. The checked-in configuration uses Cohere Transcribe 03-2026 Q8, which
is downloaded with `make download-cohere` and built with
`make build-transcribecpp`.

It participates in the shared VRAM budget and LRU eviction policy. Uploads are
normalized by the main server to mono 16 kHz PCM WAV before dispatch.

`cohere-transcribe` is deliberately buffered-only. `voxtral-realtime` uses the
same sidecar binary with transcribe.cpp's incremental stream API and is the
only checked-in provider advertised as `live_streaming`. The WebSocket holds
the GPU execution lease for the session and sends partial hypotheses after
each model update.

An optional `post_process_model` on the WebSocket creates an asynchronous
second-pass transcription job after commit. This is opt-in: Cohere is never
run automatically. The linked result is polled through
`GET /v1/audio/transcription-jobs/{id}`.

### Parakeet-TDT

**ID:** `parakeet`
**Package:** `internal/provider/parakeet`
**Runtime:** `audio.cpp` (`audiocpp-configs/parakeet.json`)

Parakeet-TDT 0.6B v3 is an available STT provider in `config.yml`. It supports
25 European languages with automatic language detection. The lifecycle manager
starts its CUDA sidecar on the first request and keeps it resident alongside
other models when the configured VRAM budget allows. The checked-in sidecar configuration uses
Q8_0 matrix weights and buffered streaming with 2-second center and right-
context windows. `stream=true` exposes its cumulative partial transcripts as
SSE.

Parakeet's multipart SSE mode remains useful for uploaded recordings, but it
is not advertised as a true live input model. The browser live microphone now
uses Voxtral Realtime over the duplex WebSocket endpoint.

Use `AUDIO_DEFAULT_STT_PROVIDER=nemotron` to make Nemotron the default without
removing Parakeet, or send `model=nemotron` on an individual request.

### faster-whisper

**ID:** `faster-whisper`  
**Package:** `internal/provider/fasterwhisper`  
**Sidecar:** `stt/fasterwhisper/server.py`  
**Requires:** Python 3.10+, `faster-whisper`, `torch` (sidecar); nothing on the Go side beyond network access to the sidecar

[faster-whisper](https://github.com/SYSTRAN/faster-whisper) (CTranslate2-based Whisper reimplementation) transcribes audio to text, with automatic language detection if `language` isn't given. The sidecar accepts raw audio bytes directly (no need to specify the format — it decodes via the same machinery Whisper/ffmpeg use internally, so WAV/MP3/etc. all work without pre-conversion).

The checked-in configuration enables it at `http://127.0.0.1:8030` with
`large-v3`, CUDA, and INT8 quantization. The Go server launches the Python
sidecar on the first request and keeps it co-resident when the configured VRAM
budget permits. Run the sidecar independently only for development:

```bash
pip install faster-whisper torch
./stt/fasterwhisper/server.py --port 8030 --model-size large-v3 --device cuda --compute-type int8
```

**Model lifecycle:** the Go lifecycle manager owns the sidecar process and
accounts for it in the same VRAM budget as OmniVoice, Supertonic, Parakeet,
and Nemotron. Its internal idle unload is disabled in managed mode; stopping
the process releases both the model and CTranslate2 CUDA allocations. In
standalone mode, the sidecar retains its own 60-second idle-unload default.

The sidecar itself returns a buffered transcription. The Go provider adapts
that result to the streaming SSE contract, allowing the web UI's cumulative
three-second microphone snapshots to update normally.

## Autodubbing analysis providers

### Sortformer

Sortformer runs through the pinned audio.cpp server using
`audiocpp-configs/sortformer.json`. It returns sample-accurate speaker turns,
including overlaps, and participates in shared CUDA lifecycle and LRU
eviction. The configured model supports at most four local speakers.

### WeSpeaker

The `wespeaker` submodule is the official native C++ ONNX Runtime. The
`speaker/wespeaker` sidecar keeps Gemini DF-ResNet114-LM resident on CPU and
returns L2-normalized 256-dimensional embeddings. It uses no configured VRAM,
so it can remain available while TTS/STT occupy the GPU.

### BS-RoFormer

BS-RoFormer runs through audio.cpp using
`audiocpp-configs/bs-roformer.json`. It is opt-in per analysis request and
isolates a vocals/dialogue stem before diarization. It does not separate one
overlapping actor from another.

## Adding a provider

1. Create a package under `internal/provider/<name>/`
2. Implement `provider.Provider`
3. Instantiate in `cmd/audio/main.go` and pass to `server.Config.Providers`
4. If the provider needs a script, model, or sidecar, put it under `tts/<name>/` (e.g. `tts/omnivoice/`, `tts/espeak-ng/`) or `stt/<name>/` for speech-to-text (e.g. `stt/fasterwhisper/`).
5. If the provider has settings beyond the shared `SpeechRequest` fields (voice/language/speed/format), define a provider-owned `Options` struct and decode it from `SpeechRequest.ProviderOptions` (`json.RawMessage`) with `DisallowUnknownFields`. Don't add provider-specific fields to the shared `SpeechRequest` type — see OmniVoice's `steps`/`seed`/`chunk_seconds`/`chunk_threshold` for the pattern.

The server routes requests by provider ID; the default provider handles all OpenAI model aliases.
