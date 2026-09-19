# Moonshine v2 sidecar (CPU only)

`server.py` exposes [Moonshine v2](https://github.com/moonshine-ai/moonshine) as
an HTTP STT sidecar, via the `moonshine-voice` pip package. The Go side is
`internal/provider/moonshine`, provider id `moonshine`.

This is **not** the GGUF `moonshine` model that audio.cpp serves on the GPU
(`audiocpp-configs/moonshine.json`). This sidecar runs entirely on the CPU:
`CUDA_VISIBLE_DEVICES` is cleared before the native library loads, and the
provider is registered with a zero VRAM cost so the residency budget never
counts or evicts it.

## Why this provider streams

Moonshine v2's encoder is designed for streaming: it caches the encoder output
and part of the decoder state and *refines* a transcript as audio arrives,
instead of re-decoding a growing buffer the way Whisper-style models must. Most
of the work therefore happens while the speaker is still talking. So the live
endpoint is the point of this provider, and `sttprovider.LiveStreamingProvider`
is implemented for real rather than faked over buffered calls.

## Setup

```bash
make setup-moonshine        # creates stt/moonshine/.venv
```

Model weights are downloaded and cached on first use, per language — nothing to
fetch ahead of time.

## Models

Moonshine publishes **one model per language**, so the requested language
selects the model. `moonshine_language` sets the default; a request's language
overrides it, and switching language swaps the resident model.

Supported: `ar de en es ja ko tl uk vi zh`.

For English the published architectures are `MEDIUM_STREAMING` (the package
default), `SMALL_STREAMING`, `TINY_STREAMING`, plus the non-streaming `BASE` and
`TINY`. **There is no `BASE_STREAMING`.** Measured on CPU over a 4.7s clip:

| arch | warm latency | vs realtime | transcript |
|---|---|---|---|
| `TINY_STREAMING` | 1.58s | 3.0x | badly garbled |
| `SMALL_STREAMING` | 2.68s | 1.7x | partly wrong |
| `MEDIUM_STREAMING` | 1.82s | 2.6x | correct |

`MEDIUM_STREAMING` is both faster and more accurate than `SMALL_STREAMING`, so
it is the default. Unlike Moonshine v1, there is no 64-second input ceiling —
an 81s recording transcribes in one call.

## HTTP surface

Buffered, matching the other STT sidecars:

| route | body | notes |
|---|---|---|
| `GET /health` | — | `{"status","model_loaded"}` |
| `POST /load` | — | optional warmup; honours `X-Transcribe-Language` |
| `POST /unload` | — | releases the resident model |
| `POST /transcribe` | 16 kHz mono PCM16 **WAV** | the Go provider declares `AudioRequirements`, so the server normalizes uploads with ffmpeg first |

Live, matching the protocol `transcribe.cpp`'s sidecar already speaks. Only one
session exists at a time; the Go side serializes access:

| route | body | notes |
|---|---|---|
| `POST /stream/begin` | — | `X-Transcribe-Language` selects the model |
| `POST /stream/feed` | raw PCM16 frames | returns an incremental update |
| `POST /stream/finalize` | — | flushes and returns the final transcript |
| `POST /stream/reset` | — | discards the session |

A live update reports `text`, `committed_text`, `tentative_text`, `input_ms`,
`buffered_ms`, `revision`, `changed`, and `final`. Committed text comes from
lines Moonshine has closed and will not revise; the trailing open line is
tentative.

## Options

`--threads` bounds ONNX Runtime's intra-op pool (0 = all physical cores) — worth
setting if the machine is busy with other work. `--option key=value` is a
passthrough to the underlying C API for things the Python layer does not wrap,
for example `--option word_timestamps=true` or `--option identify_speakers=true`.
`--idle-unload-seconds` releases the model when unused; it never fires while a
live session is open.
