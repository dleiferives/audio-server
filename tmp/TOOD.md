# Near-term TODO

## Normalize STT audio before provider dispatch

The audio server should accept common uploaded audio formats and convert them
to the format required by the selected STT provider. Clients should not need to
know that a backend such as Parakeet requires mono, 16 kHz PCM.

- Decode uploads with the existing ffmpeg integration.
- Define each STT provider's required sample rate, channel count, sample format,
  and container through provider capabilities instead of hard-coding Parakeet
  behavior in the HTTP handler.
- Normalize once in the server before buffered or streaming transcription
  dispatch while preserving the original filename/format when conversion is
  unnecessary.
- Return a clear `400` error for corrupt or unsupported input rather than a
  provider-level `503` error.
- Add coverage for WAV inputs at 16 kHz and 22.05/44.1/48 kHz, stereo audio,
  and compressed formats such as MP3, Ogg/Opus, FLAC, and M4A where ffmpeg can
  decode them.
- Verify the same normalized audio works with Parakeet, Nemotron, Qwen3-ASR,
  and faster-whisper without changing the public transcription API.

Acceptance criterion: uploading valid audio in any supported container results
in a transcription request to the chosen provider in that provider's required
native format, with no format preparation required by the caller.

## Add complete interactive API documentation

Publish a complete OpenAPI 3.1 description of the audio server and serve an
interactive API documentation page from the running service.

- Serve the machine-readable specification at `/openapi.json`.
- Serve an interactive Swagger UI, Scalar, or ReDoc page at `/docs`; prefer a
  self-contained or locally bundled UI so documentation works without an
  external CDN.
- Document bearer-token authentication and clearly identify endpoints that do
  not require authentication, such as health checks.
- Cover speech synthesis, streaming speech, transcription, live/streaming STT,
  voices, capabilities, asynchronous jobs and polling, stored audio, forced
  alignment, and health endpoints.
- Describe every request and response schema, supported content type, multipart
  field, query parameter, status code, and structured error response.
- Explain provider/model routing, OpenAI-compatible aliases, automatic
  language/voice selection, provider-specific options, lifecycle-managed cold
  starts, queue positions, timeouts, and audio retention.
- Include copyable examples for curl and JSON/multipart requests, plus examples
  of SSE events and downloading binary audio responses.
- Generate provider, language, voice, and model capability examples from the
  server's actual registered providers where practical so the docs do not
  silently drift from runtime behavior.
- Add automated validation for the OpenAPI document and tests asserting that
  `/openapi.json` and `/docs` are served successfully.
- Keep `docs/api.md` as the longer conceptual guide, with links in both
  directions between it and the interactive reference.

Acceptance criterion: a new user can open `/docs`, understand the service's
architecture and authentication, exercise every public API workflow, and build
a client without reading the Go source.
