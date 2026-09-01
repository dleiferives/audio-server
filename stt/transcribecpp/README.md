# transcribe.cpp sidecar

This native C++ sidecar keeps one [transcribe.cpp](../../transcribe.cpp) GGUF
model resident and exposes it to the Go server. It accepts any model family
supported by the pinned transcribe.cpp revision; the default configuration
uses Cohere Transcribe 03-2026 Q8.

Build the CUDA runtime and sidecar, then download the default model:

```bash
make build-transcribecpp
make download-cohere
make download-voxtral
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

## True live streaming

Voxtral Realtime is exposed as `voxtral-realtime` through
`GET /v1/audio/transcriptions/stream`, a WebSocket endpoint. Send mono 16 kHz
PCM16LE binary frames and finish with `{"type":"input_audio.commit"}`.

Add `post_process_model=cohere-transcribe` to the WebSocket query only when a
separate, asynchronous Cohere final pass is desired. The server then emits a
linked transcription job ID; it never runs this extra pass implicitly.
