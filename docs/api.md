# API reference

The server exposes an OpenAI-compatible TTS API on `http://127.0.0.1:8010` by default.

## Endpoints

### `GET /healthz`

Returns provider health status. No authentication required.

**Response 200**
```json
{
  "status": "ok",
  "providers": {
    "espeak-ng": "ok"
  }
}
```

**Response 503** — one or more providers are unavailable:
```json
{
  "status": "unhealthy",
  "providers": {
    "espeak-ng": "espeak-ng not found"
  }
}
```

---

### `GET /v1/audio/voices`

List available voices, optionally filtered by language.

**Query parameters**

| Parameter | Description |
|---|---|
| `language` | BCP-47 language tag filter (e.g. `en`, `el`, `fr`) |
| `provider` | provider ID to query (default: the default provider) |

**Response 200**
```json
{
  "voices": [
    {
      "provider": "espeak-ng",
      "voice": "en-us",
      "language": "en/en-us",
      "name": "en-us",
      "gender": "M"
    }
  ]
}
```

---

### `POST /v1/audio/speech`

Synthesize speech from text and wait for the result. Returns raw audio bytes.

Internally this submits a job to the same per-provider queue used by
`/v1/audio/jobs` and blocks until it finishes (bounded by
`AUDIO_REQUEST_TIMEOUT_SECONDS`). If the provider is busy — e.g. OmniVoice
processing an earlier request — this call simply waits behind it; use
`/v1/audio/jobs` instead if you want to see queue position rather than block.

**Note:** if this HTTP request times out or the client disconnects, the job
keeps running in the background — it isn't cancelled — since other queued
jobs and any later poll depend on it completing.

**Request body**
```json
{
  "model": "tts-1",
  "input": "hello world",
  "voice": "en-us",
  "language": "en",
  "response_format": "mp3",
  "speed": 1.0
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `model` | string | no | Provider to use. `tts-1`, `tts-1-hd`, `gpt-4o-mini-tts`, `auto`, or blank all map to the default provider. Use the provider ID (e.g. `espeak-ng`) to target a specific one. |
| `input` | string | yes | Text to synthesize. Max `AUDIO_MAX_INPUT_CHARS` characters. |
| `voice` | string | no | Voice name. Provider-specific. Falls back to `AUDIO_ESPEAK_DEFAULT_VOICE`. |
| `language` | string | no | BCP-47 language tag. Used as fallback voice when `voice` is blank. |
| `response_format` | string | no | `mp3` (default), `wav`, `ogg`, `opus`, or `flac`. |
| `speed` | float | no | Playback speed multiplier. Range 0.25–4.0. |
| `provider_options` | object | no | Provider-specific settings, opaque to the server and validated only by the resolved provider. See [`docs/providers.md`](providers.md) for each provider's schema (e.g. OmniVoice's `steps`, `seed`, `chunk_seconds`, `chunk_threshold`). Unknown fields within it are rejected by the provider, not the server. |
| `stream` | boolean | no | Stream audio progressively instead of buffering the full result. Only honored if the resolved provider supports it (currently `espeak-ng` only) — otherwise silently falls back to the buffered response below. See "Streaming" below. |

**Response 200** — audio bytes with headers:

| Header | Description |
|---|---|
| `Content-Type` | `audio/mpeg`, `audio/wav`, `audio/ogg`, `audio/ogg; codecs=opus`, or `audio/flac` |
| `X-TTS-Provider` | Provider that handled the request |
| `X-TTS-Model` | Model/provider ID echoed back |
| `X-TTS-Voice` | Voice actually used |
| `X-TTS-Format` | Format actually used |

**Error responses**
```json
{ "error": "input is required" }
```

| Status | Condition |
|---|---|
| 400 | Invalid request, unsupported format, unknown model, or invalid `provider_options` |
| 401 | Missing or invalid API key (when `AUDIO_API_KEY` is set) |
| 503 | Provider unavailable or synthesis failed |
| 504 | Timed out waiting for the job (the job itself may still complete in the background) |

#### Streaming (`"stream": true`)

```bash
curl -sS --no-buffer http://127.0.0.1:8010/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"input":"a longer piece of text ...","voice":"en-us","response_format":"mp3","stream":true}' \
  --output out.mp3
```

When streaming is used, audio bytes are written and flushed to the response as they're produced — playback can start before generation finishes, instead of waiting for the whole file. Behavior differs from the buffered path in one important way: **streaming bypasses the job queue entirely** and is tied directly to this HTTP connection, so if the client disconnects or the request times out, synthesis is actually cancelled (there's no other consumer waiting on a streamed response, unlike a queued job). Headers (`X-TTS-*`, `Content-Type`) are still sent up front, before any audio bytes, since voice/format are resolved synchronously from the request.

If the resolved provider doesn't support streaming (e.g. `omnivoice`), `stream` is silently ignored and the normal buffered response is returned instead.

---

### `POST /v1/audio/jobs`

Submit a synthesis job and return immediately — use this instead of
`/v1/audio/speech` when you want to see queue position rather than hold a
connection open, which matters most for slow GPU-bound providers like
OmniVoice.

**Request body:** identical to `POST /v1/audio/speech` (see above), including
`provider_options`. **`stream` is not supported here** — jobs are fetch-after-done
by design, and a `stream: true` job request returns `400`. Use
`POST /v1/audio/speech` for streaming.

**Response 202**
```json
{
  "id": "d094191e932dd2af2d9569f5417c0158",
  "status": "queued",
  "provider": "omnivoice",
  "created_at": "2026-07-09T00:51:26.388Z",
  "queue_position": 2
}
```

`queue_position` is 1-based among still-queued jobs for that provider; it's
omitted (implicitly `0`) once the job starts running.

---

### `GET /v1/audio/jobs/{id}`

Poll a job's status.

**Response 200**
```json
{
  "id": "d094191e932dd2af2d9569f5417c0158",
  "status": "running",
  "provider": "omnivoice",
  "created_at": "2026-07-09T00:51:26.388Z",
  "started_at": "2026-07-09T00:51:26.420Z",
  "queue_position": 0
}
```

| `status` | Meaning |
|---|---|
| `queued` | Waiting for a worker; `queue_position` reflects its place in line. |
| `running` | Actively synthesizing (this also covers OmniVoice model warm-up on a cold start — there's no separate "loading" state). |
| `succeeded` | Done; fetch audio from `GET /v1/audio/jobs/{id}/audio`. |
| `failed` | `error` field holds the failure reason. |

**Response 404** — unknown or expired job ID. Finished jobs are retained for
about 10 minutes before being swept from memory; a server restart also loses
all jobs (in-memory only, not persisted).

---

### `GET /v1/audio/jobs/{id}/audio`

Fetch the result of a finished job. Same response headers as
`POST /v1/audio/speech`.

| Status | Condition |
|---|---|
| 200 | Job succeeded; audio bytes returned. |
| 404 | Unknown job ID, or the job failed (see `GET /v1/audio/jobs/{id}` for the error). |
| 409 | Job exists but hasn't finished yet — poll `GET /v1/audio/jobs/{id}` first. |

## Authentication

When `AUDIO_API_KEY` is set, all `/v1/` endpoints require one of:

- `Authorization: Bearer <key>` header
- `X-API-Key: <key>` header

The `/healthz` endpoint is always unauthenticated.
