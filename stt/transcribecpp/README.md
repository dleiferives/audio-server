# transcribe.cpp sidecar

This native C++ sidecar keeps one [transcribe.cpp](../../transcribe.cpp) GGUF
model resident and exposes it to the Go server. It accepts any model family
supported by the pinned transcribe.cpp revision; the default configuration
uses Cohere Transcribe 03-2026 Q8.

Build the CUDA runtime and sidecar, then download the default model:

```bash
make build-transcribecpp
make download-cohere
```

The main audio server starts the sidecar on the first matching STT request.
To run it directly:

```bash
./bin/transcribecpp_server \
  --model models/cohere-transcribe-03-2026-Q8_0.gguf \
  --backend cuda --host 127.0.0.1 --port 8031
```

The audio server normalizes WAV, MP3, Opus, and other ffmpeg-decodable uploads
to the mono 16 kHz WAV input required by transcribe.cpp.
