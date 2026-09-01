# WeSpeaker native speaker embeddings

This sidecar keeps the official WeSpeaker C++ ONNX runtime and the configured
speaker model resident on CPU. The main audio server normalizes uploads to
mono 16 kHz PCM16 WAV before calling it.

Build and download the pinned Gemini DF-ResNet114-LM ONNX model:

```bash
make build-wespeaker download-wespeaker
```

Standalone run:

```bash
bin/wespeaker_server \
  --model models/wespeaker/voxceleb_gemini_dfresnet114_LM.onnx \
  --host 127.0.0.1 --port 8034
```

The sidecar accepts a normalized WAV body at `POST /embed`. Most callers
should use the public audio-server analysis jobs instead of calling the
sidecar directly.
