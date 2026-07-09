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

The Go provider is a thin HTTP client: it calls `GET /health`, `GET /voices`, and `POST /synthesize` on the sidecar. Enable it by setting `AUDIO_OMNIVOICE_ADDR` (or `-omnivoice-addr`) to the sidecar's base URL, e.g. `http://127.0.0.1:8020`; the provider is not registered when this is blank, so the main server starts fine without the sidecar running.

Run the sidecar independently:

```bash
python -m pip install torch==2.8.0+cu128 torchaudio==2.8.0+cu128 \
  --extra-index-url https://download.pytorch.org/whl/cu128
python -m pip install omnivoice==0.1.5
./tts/omnivoice/server.py --port 8020
```

### Routing

- `"model": "omnivoice"` routes directly to this provider
- `"language": "el"` with no explicit model also routes here (see `languageProviders` in `internal/server/server.go`)

See [`tts/omnivoice/README.md`](../tts/omnivoice/README.md) for the standalone CLI script (`greek_tts.py`), which the sidecar's model-loading logic is based on.

## Adding a provider

1. Create a package under `internal/provider/<name>/`
2. Implement `provider.Provider`
3. Instantiate in `cmd/audio/main.go` and pass to `server.Config.Providers`
4. If the provider needs a script, model, or sidecar, put it under `tts/<name>/` (e.g. `tts/omnivoice/`, `tts/espeak-ng/`). Future STT/diarization providers follow the same pattern under `stt/<name>/`.

The server routes requests by provider ID; the default provider handles all OpenAI model aliases.
