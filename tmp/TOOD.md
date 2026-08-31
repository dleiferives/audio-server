# Near-term TODO

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
