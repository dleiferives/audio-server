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

Every request runs through `internal/queue`, which gives each provider its own FIFO queue and worker pool (see `docs/architecture.md`). `Synthesize` itself is unchanged by this — a provider still just does one request in, one result out.

### Optional: `provider.Lifecycle`

```go
type Lifecycle interface {
    Warm(ctx context.Context) error
    Idle(ctx context.Context) error
}
```

Providers with an expensive resource to manage (a GPU-resident model) can implement this. The queue manager calls `Warm` before the first job dispatched to a cold provider, and `Idle` after that provider's queue has been empty for its configured idle-unload delay — so the model loads once and stays loaded across a run of back-to-back jobs, rather than reloading per request. Providers that don't implement it (like `espeak-ng`) are unaffected — the manager just skips the lifecycle calls.

## espeak-ng

**ID:** `espeak-ng`  
**Package:** `internal/provider/espeak`  
**Requires:** `espeak-ng` binary, `ffmpeg` binary (for MP3 encoding)

See [`tts/espeak-ng/README.md`](../tts/espeak-ng/README.md) for setup notes.

The espeak provider shells out to `espeak-ng --stdin --stdout` to generate WAV, then pipes the result through `ffmpeg` to produce MP3. WAV output is returned directly without encoding.

### Voice selection

1. Use `voice` from the request if set and not `"auto"`
2. Use `language` from the request (normalized to BCP-47, e.g. `en-us`)
3. Fall back to `AUDIO_ESPEAK_DEFAULT_VOICE` (default: `en`)

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
}
```

The shipped implementation is `FFmpeg` (`internal/encode/ffmpeg.go`).

## omnivoice

**ID:** `omnivoice`  
**Package:** `internal/provider/omnivoice`  
**Sidecar:** `tts/omnivoice/server.py`  
**Requires:** Python 3.10+, CUDA-enabled PyTorch, `omnivoice==0.1.5` (sidecar); nothing on the Go side beyond network access to the sidecar

The OmniVoice model (`k2-fsa/OmniVoice`) handles Greek (`el`) synthesis via diffusion. It chunks long input internally and reuses the first generated voice for consistency. WAV output only (no format conversion).

The Go provider is a thin HTTP client: it calls `GET /health`, `GET /voices`, `POST /synthesize`, and (via `provider.Lifecycle`) `POST /load` / `POST /unload` on the sidecar. Enable it by setting `AUDIO_OMNIVOICE_ADDR` (or `-omnivoice-addr`) to the sidecar's base URL, e.g. `http://127.0.0.1:8020`; the provider is not registered when this is blank, so the main server starts fine without the sidecar running.

Run the sidecar independently:

```bash
python -m pip install torch==2.8.0+cu128 torchaudio==2.8.0+cu128 \
  --extra-index-url https://download.pytorch.org/whl/cu128
python -m pip install omnivoice==0.1.5
./tts/omnivoice/server.py --port 8020
```

### GPU model lifecycle (VRAM budget)

The sidecar doesn't load the model at startup — it loads lazily, on the queue manager's first `Warm` call (or on the first direct `/synthesize` call, defensively). This matters on VRAM-constrained boxes: the model stays resident while OmniVoice jobs keep arriving, and gets unloaded (`torch.cuda.empty_cache()`) once its queue has been empty for `AUDIO_OMNIVOICE_IDLE_UNLOAD_SECONDS` (default 30s). `AUDIO_OMNIVOICE_CONCURRENCY` (default **1**) caps how many OmniVoice jobs run at once — keep this at 1 on a single GPU with limited VRAM, since the sidecar has no concept of splitting VRAM across concurrent generations. Pass `--preload` / `OMNIVOICE_PRELOAD=1` to the sidecar if you'd rather it load eagerly at startup (e.g. for a dedicated GPU box where idle-unload isn't needed).

### Routing

- `"model": "omnivoice"` routes directly to this provider
- `"language": "el"` with no explicit model also routes here (see `languageProviders` in `internal/server/server.go`)

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

## Adding a provider

1. Create a package under `internal/provider/<name>/`
2. Implement `provider.Provider`
3. Instantiate in `cmd/audio/main.go` and pass to `server.Config.Providers`
4. If the provider needs a script, model, or sidecar, put it under `tts/<name>/` (e.g. `tts/omnivoice/`, `tts/espeak-ng/`). Future STT/diarization providers follow the same pattern under `stt/<name>/`.
5. If the provider has settings beyond the shared `SpeechRequest` fields (voice/language/speed/format), define a provider-owned `Options` struct and decode it from `SpeechRequest.ProviderOptions` (`json.RawMessage`) with `DisallowUnknownFields`. Don't add provider-specific fields to the shared `SpeechRequest` type — see OmniVoice's `steps`/`seed`/`chunk_seconds`/`chunk_threshold` for the pattern.

The server routes requests by provider ID; the default provider handles all OpenAI model aliases.
