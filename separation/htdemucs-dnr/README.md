# HTDemucs-DnR cinematic separation sidecar

`server.py` is a minimal HTTP sidecar around a DnR (Divide and Remaster)
trained HTDemucs checkpoint, run through the real PyTorch
[demucs](https://github.com/facebookresearch/demucs) package. Unlike
`bs-roformer`/`htdemucs` (native audio.cpp GGUF, no Python), this one exists
specifically because audio.cpp's C++ HTDemucs port can't run this
checkpoint — see [`tmp/TOOD.md`](../../tmp/TOOD.md) for why (it has
`bottom_channels=0`, a valid "skip the bottleneck projection" mode that
audio.cpp doesn't handle). Running it through the actual training-time
framework sidesteps that entirely.

It exposes `GET /health`, `POST /load`, `POST /unload`, and
`POST /v1/tasks/run` — the same request/response shape as audio.cpp's
`/v1/tasks/run` (`{"model", "request": {"audio": <path>}}` →
`{"named_audio_outputs": [{"id", "audio": <base64 wav>}]}`), so the Go
provider (`internal/provider/htdemucsdnr`) is a near-verbatim copy of the
native `htdemucs` provider pointed at a different port.

Set up the env:

```bash
cd separation/htdemucs-dnr
uv venv .venv --python 3.11
uv pip install --python .venv/bin/python torch torchaudio demucs numpy soundfile
```

Download the checkpoint (not bundled — 51MB, a `Demucs4`/HTDemucs model
from [ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing](https://github.com/ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing),
release `v.1.0.0`, trained for the SDX'23 Cinematic Demixing track):

```bash
mkdir -p models
curl --fail --location --output models/97d170e1-dbb4db15.th \
  "https://github.com/ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing/releases/download/v.1.0.0/97d170e1-dbb4db15.th"
```

Run the sidecar:

```bash
.venv/bin/python server.py --port 8041 --device cuda
```

`--device auto` (default) picks CUDA if available, else CPU. By default the
model loads lazily on first request; pass `--preload` (or set
`HTDEMUCS_DNR_PRELOAD=1`) to load it at startup instead.

Outputs 3 stems keyed as `music`, `effects`, `dialogue` (mapped from the
checkpoint's own `sources` order — `music`, `sfx`, `speech`).
