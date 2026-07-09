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

## OmniVoice (Greek)

**Status:** standalone script, not yet integrated as a provider  
**Location:** `tts/el/greek_tts.py`  
**Requires:** Python 3.10+, CUDA-enabled PyTorch, `omnivoice==0.1.5`

The OmniVoice model (`k2-fsa/OmniVoice`) handles Greek (`el`) synthesis via diffusion. It chunks long input internally and reuses the first generated voice for consistency.

See [`tts/el/README.md`](../tts/el/README.md) for usage.

The planned integration path is an HTTP sidecar: the Python script exposes a local HTTP endpoint that the Go server calls as a provider backend. See the open GitHub issue.

## Adding a provider

1. Create a package under `internal/provider/<name>/`
2. Implement `provider.Provider`
3. Instantiate in `cmd/audio/main.go` and pass to `server.Config.Providers`

The server routes requests by provider ID; the default provider handles all OpenAI model aliases.
