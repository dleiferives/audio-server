# CosyVoice2 voice-cloning sidecar

`server.py` is a minimal HTTP sidecar around
[CosyVoice2](https://github.com/FunAudioLLM/CosyVoice) (FunAudioLLM), a
multilingual (zh/en/ja/ko + Chinese dialects) zero-shot voice-cloning TTS
model. Its zero-shot mode conditions on an audio prompt *plus its matching
transcript* (`reference_text`), unlike Chatterbox. It exposes
`POST /synthesize`, `GET /health`, `POST /load`, and `POST /unload` — see
[`docs/providers.md`](../../docs/providers.md) for the full contract. It's a
fallback voice-cloning provider alongside OmniVoice and Chatterbox.

CosyVoice isn't published as an installable package, so this needs a repo
checkout on `PYTHONPATH` in addition to a venv:

```bash
cd tts/cosyvoice
git clone --recurse-submodules https://github.com/FunAudioLLM/CosyVoice.git repo
uv venv .venv --python 3.10
uv pip install --python .venv/bin/python setuptools wheel
uv pip install --python .venv/bin/python -r requirements-inference.txt \
  --index-strategy unsafe-best-match --no-build-isolation-package openai-whisper
```

`requirements-inference.txt` is `repo/requirements.txt` trimmed of
training/export/demo-only deps (TensorRT, DeepSpeed, Gradio, fastapi-cli,
grpcio-tools, gdown, wget, tensorboard) that aren't needed to just serve
inference over our own minimal HTTP server.

On a Blackwell GPU (sm_120, e.g. RTX 50-series) the repo's pinned
`torch==2.3.1`/`torchaudio==2.3.1` (cu121) lack Blackwell kernels and fail at
inference time with `CUDA error: no kernel image is available`. Override
them after the requirements install:

```bash
uv pip install --python .venv/bin/python -U torch torchaudio
uv pip install --python .venv/bin/python "setuptools<81"  # the upgrade above reinstalls a newer setuptools; repin it
```

Download the model weights (not bundled — pick one):

```bash
# via Hugging Face
.venv/bin/python -c "from huggingface_hub import snapshot_download; \
  snapshot_download('FunAudioLLM/CosyVoice2-0.5B', local_dir='pretrained_models/CosyVoice2-0.5B')"

# or via ModelScope (repo's documented default)
.venv/bin/python -c "from modelscope import snapshot_download; \
  snapshot_download('iic/CosyVoice2-0.5B', local_dir='pretrained_models/CosyVoice2-0.5B')"
```

Run the sidecar:

```bash
.venv/bin/python server.py --port 8038 --device cuda \
  --repo-path repo --model-dir pretrained_models/CosyVoice2-0.5B
```

By default the model loads lazily on first request; pass `--preload` (or set
`COSYVOICE_PRELOAD=1`) to load it at startup instead.

`/synthesize` takes `{"text", "reference_audio_path", "reference_text",
"speed", "seed"}` — both `reference_audio_path` (a local WAV file, same-host
subprocess) and `reference_text` (a transcript of that clip) are required;
CosyVoice2's zero-shot prompt needs the matching text to condition on.
