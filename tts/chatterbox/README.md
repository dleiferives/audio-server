# Chatterbox voice-cloning sidecar

`server.py` is a minimal HTTP sidecar around
[Chatterbox](https://github.com/resemble-ai/chatterbox) (resemble-ai), a
zero-shot voice-cloning TTS model. It clones from an audio prompt alone —
no matching transcript is required, unlike CosyVoice2's zero-shot mode.
It exposes `POST /synthesize`, `GET /health`, `POST /load`, and
`POST /unload` — see [`docs/providers.md`](../../docs/providers.md) for the
full contract and how the Go server drives it. It's a fallback voice-cloning
provider alongside OmniVoice and CosyVoice2.

Set up the env (already done for this checkout via `uv`):

```bash
cd tts/chatterbox
uv venv .venv --python 3.12
uv pip install --python .venv/bin/python torch torchaudio chatterbox-tts "setuptools<81"
```

`setuptools<81` is required because `resemble-perth` (a chatterbox-tts dep)
still imports the now-removed `pkg_resources` at runtime.

chatterbox-tts pins `torch==2.6.0`, which on a Blackwell GPU (sm_120, e.g.
RTX 50-series) lacks Blackwell kernels and fails at inference time with
`CUDA error: no kernel image is available`. Override it after install:

```bash
uv pip install --python .venv/bin/python -U torch torchaudio
```

Chatterbox downloads model weights lazily from Hugging Face on first use.

Run the sidecar:

```bash
.venv/bin/python server.py --port 8037 --device cuda
```

`--device auto` (default) picks CUDA if available, then MPS, then falls back
to CPU.

By default the model loads lazily on first request; pass `--preload` (or set
`CHATTERBOX_PRELOAD=1`) to load it at startup instead.

`/synthesize` takes `{"text", "reference_audio_path", "exaggeration",
"cfg_weight", "seed"}` — `reference_audio_path` must be a local WAV file
path (the Go server writes the uploaded speaker reference clip to a temp
file before calling this, since it's a same-host subprocess).
