# Kokoro TTS sidecar

`server.py` is a minimal HTTP sidecar around [Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M),
a small, fast, multi-voice TTS model. It exposes `POST /synthesize`,
`GET /voices`, `GET /health`, `POST /load`, and `POST /unload` — see
[`docs/providers.md`](../../docs/providers.md#kokoro) for the full contract
and how the Go server drives it.

Install in a Python 3.10+ environment:

```bash
pip install torch kokoro soundfile
# English G2P frontend (used by lang_code 'a'/'b'):
pip install misaki[en]
```

Kokoro downloads model weights and voice packs lazily from Hugging Face on
first use.

Run the sidecar:

```bash
./server.py --port 8021
```

`--device auto` (default) picks CUDA if available, then MPS, then falls back
to CPU — Kokoro is small enough (~82M params) to run reasonably on CPU too,
unlike the GPU-only OmniVoice provider.

By default the model loads lazily on first request; pass `--preload` (or set
`KOKORO_PRELOAD=1`) to load it eagerly at startup instead.
