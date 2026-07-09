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

Synthesize speech from text. Returns raw audio bytes.

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
| `response_format` | string | no | `mp3` (default) or `wav`. |
| `speed` | float | no | Playback speed multiplier. Range 0.25–4.0. |
| `provider_options` | object | no | Provider-specific settings, opaque to the server and validated only by the resolved provider. See [`docs/providers.md`](providers.md) for each provider's schema (e.g. OmniVoice's `steps`, `seed`, `chunk_seconds`, `chunk_threshold`). Unknown fields within it are rejected by the provider, not the server. |

**Response 200** — audio bytes with headers:

| Header | Description |
|---|---|
| `Content-Type` | `audio/mpeg` or `audio/wav` |
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
| 504 | Synthesis timed out |

## Authentication

When `AUDIO_API_KEY` is set, all `/v1/` endpoints require one of:

- `Authorization: Bearer <key>` header
- `X-API-Key: <key>` header

The `/healthz` endpoint is always unauthenticated.
