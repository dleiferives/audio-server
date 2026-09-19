# API reference

The server exposes an OpenAI-compatible TTS API on `http://127.0.0.1:8010` by default.

The running server also exposes the machine-readable OpenAPI 3.1 contract at
`/openapi.json` and a self-contained interactive explorer at `/docs`. This file
is the longer conceptual guide; the OpenAPI document is the authoritative
operation and schema reference.

## Endpoints

### `GET /healthz`

Returns provider health status. No authentication required. Lifecycle-managed
providers may report `cold` when their sidecar is intentionally stopped; that
is a ready-to-start state, not an outage, and does not make the endpoint fail.

**Response 200**
```json
{
  "status": "ok",
  "providers": {
    "espeak-ng": "ok"
  }
}
```

Example with an unloaded GPU provider:

```json
{
  "status": "ok",
  "providers": {
    "espeak-ng": "ok",
    "supertonic": "ok",
    "omnivoice": "cold"
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
| `language` | BCP-47 language tag filter (e.g. `en`, `el`, `fr`); use `auto` for the provider's automatic/default language behavior |
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

### `GET /v1/audio/capabilities`

Returns the complete TTS and STT catalog exposed by this audio-server instance. The
top-level `languages` list is the union across providers. Each provider lists
its supported languages and canonical voice definitions. To get voices
compatible with one selected language, call `/v1/audio/voices` with both
`provider` and `language`.

**Response 200**
```json
{
  "languages": ["ar", "el", "en", "ja"],
  "providers": [
    {
      "provider": "supertonic",
      "languages": ["ar", "el", "en", "ja"],
      "voices": [
        {"provider": "supertonic", "voice": "M1", "language": "en", "name": "Male 1"}
      ]
    }
  ],
  "stt_providers": [
    {"provider": "cohere-transcribe", "live_streaming": false},
    {"provider": "voxtral-realtime", "live_streaming": true}
  ]
}
```

`voice=auto` is the provider-independent default voice value. It is not a
provider voice; it asks a provider that supports the selected language to
choose its default voice.

---

### `POST /v1/audio/speech`

Synthesize speech from text and wait for the result. Returns raw audio bytes.

Internally this submits a job to the same per-provider queue used by
`/v1/audio/jobs` and blocks until it finishes (bounded by
`AUDIO_REQUEST_TIMEOUT_SECONDS`). If the provider is busy — e.g. OmniVoice
processing an earlier request, or waiting for Supertonic to release the shared
GPU — this call simply waits behind it; use
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
| `voice` | string | no | Provider voice name, or `auto` (the default) to let the provider choose. Explicit voices must support `language`. |
| `language` | string | no | BCP-47 language tag, or `auto` (the default when omitted) when supported by the selected provider. The server uses it to filter eligible voices. |
| `response_format` | string | no | `mp3` (default), `wav`, `ogg`, `opus`, `flac`, or raw `pcm`. |
| `speed` | float | no | Playback speed multiplier. Range 0.25–4.0. |
| `provider_options` | object | no | Provider-specific settings, opaque to the server and validated only by the resolved provider. See [`docs/providers.md`](providers.md) for each provider's schema (e.g. OmniVoice's `steps`, `seed`, `chunk_seconds`, `chunk_threshold`). Unknown fields within it are rejected by the provider, not the server. |
| `stream` | boolean | no | Stream audio progressively instead of buffering the full result. Only honored if the resolved provider supports it (currently `espeak-ng` only) — otherwise silently falls back to the buffered response below. See "Streaming" below. |

For per-request voice cloning, the same endpoint also accepts
`multipart/form-data`. OmniVoice currently supports a combined speaker and
emotion reference: the uploaded performance supplies both the voice identity
and its speaking style. Common compressed audio formats are decoded by FFmpeg
and normalized automatically.

```bash
curl -sS http://127.0.0.1:8010/v1/audio/speech \
  -F model=omnivoice \
  -F input='Γεια σου, πώς είσαι;' \
  -F language=el \
  -F response_format=wav \
  -F speaker_reference=@reference.m4a \
  -F speaker_reference_text='The exact words spoken in reference.m4a' \
  -F 'provider_options={"steps":32,"seed":42}' \
  --output cloned.wav
```

`speaker_reference_text` is required because OmniVoice conditions on the
matching reference transcript. The upload is scoped to this request and is
not retained as a reusable voice ID. A separate `emotion_reference` upload is
reserved for future providers and currently returns `400`.

**Response 200** — audio bytes with headers:

| Header | Description |
|---|---|
| `Content-Type` | `audio/mpeg`, `audio/wav`, `audio/ogg`, `audio/ogg; codecs=opus`, `audio/flac`, or `audio/pcm` |
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

Audio bytes are written and flushed to the response as they're produced — playback can start before generation finishes. Stateless providers stream directly; lifecycle-managed providers first enter the queue and wait for warm-up/shared-resource scheduling, then stream through the same live response. Disconnecting the client cancels the active stream. Headers (`X-TTS-*`, `Content-Type`) are sent when the provider begins producing audio.

If the resolved provider doesn't support streaming, `stream` is silently ignored and the normal buffered response is returned instead. OmniVoice and Supertonic use audio.cpp's streaming mode when configured; their SSE events are decoded by audio-server into raw PCM chunks for the client.

---

### `POST /v1/audio/jobs`

Submit a synthesis job and return immediately — use this instead of
`/v1/audio/speech` when you want to see queue position rather than hold a
connection open, which matters most for slow GPU-bound providers like
OmniVoice.

**Request body:** identical to `POST /v1/audio/speech` (see above), including
multipart speaker-reference uploads and `provider_options`. **`stream` is not supported here** — jobs are fetch-after-done
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
| `queued` | Waiting for a provider worker or for the shared resource handoff; `queue_position` reflects its place in the provider queue. |
| `running` | Actively warming or synthesizing. A cold sidecar is started only after its job reaches the shared resource. |
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

---

### `POST /v1/audio/transcriptions`

Speech-to-text, OpenAI-compatible: `multipart/form-data`, synchronous (no job queue involved — see `docs/providers.md` for why STT doesn't route through `internal/queue` the way TTS does). Cohere Transcribe is the default provider in `config.yml`; the endpoint returns `503` if no STT provider is configured.

```bash
curl -sS http://127.0.0.1:8010/v1/audio/transcriptions \
  -F file=@speech.wav \
  -F language=en
```

**Form fields**

| Field | Required | Description |
|---|---|---|
| `file` | yes | Audio file. Common formats supported by ffmpeg are accepted, including WAV, MP3, Ogg/Opus, FLAC, and M4A. The server automatically converts audio to the selected provider's required sample rate, channel count, codec, and container. |
| `model` | no | Provider ID (`cohere-transcribe`, `voxtral-realtime`, `parakeet`, `nemotron`, or `faster-whisper`). `whisper-1`, `auto`, or blank map to the configured default. |
| `language` | no | ISO-639-1/BCP-47 language hint (e.g. `en`). Omit to let the model auto-detect. |
| `response_format` | no | `json` (default, `{"text": "..."}`) or `text` (plain body). |
| `stream` | no | `true` emits cumulative partial transcripts as server-sent events. Requires a streaming-capable provider such as Parakeet. |

**Response 200**
```json
{ "text": "The quick brown fox jumps over the lazy dog." }
```

**Streaming response**

```bash
curl -N http://127.0.0.1:8010/v1/audio/transcriptions \
  -F file=@speech.wav \
  -F model=parakeet \
  -F stream=true
```

The response emits cumulative `transcript.text.delta` events, one
`transcript.text.done` event, and `data: [DONE]`.

| Status | Condition |
|---|---|
| 400 | Missing, corrupt, or unsupported audio; invalid `response_format`; or unknown `model` |
| 401 | Missing or invalid API key (when `AUDIO_API_KEY` is set) |
| 503 | No STT provider configured, or the provider is unavailable |
| 504 | Timed out |

---

### `GET /v1/audio/transcriptions/stream` (WebSocket)

True duplex live transcription. This route upgrades to WebSocket and accepts
only models whose runtime advertises native incremental streaming. Selecting
an offline model such as `cohere-transcribe` returns `400`; the server never
pretends that repeated full-file decoding is live streaming.

Query parameters:

| Parameter | Required | Description |
|---|---|---|
| `model` | yes | A live model, currently `voxtral-realtime`. |
| `language` | no | Optional BCP-47 hint. Voxtral Realtime auto-detects. |
| `post_process_model` | no | Explicitly request a separate buffered final pass, for example `cohere-transcribe`. If omitted, no second pass or job is created. |

After the `transcription_session.created` event, send binary WebSocket frames
containing mono 16 kHz signed PCM16 little-endian audio. The server emits
`transcript.text.partial` snapshots as the model advances. Finish with:

```json
{"type":"input_audio.commit"}
```

The final live event is `transcript.text.done`. When `post_process_model` was
requested, it is followed by:

```json
{
  "type": "transcription.post_process.created",
  "job": {
    "id": "stt_…",
    "model": "cohere-transcribe",
    "status": "queued",
    "status_url": "/v1/audio/transcription-jobs/stt_…"
  }
}
```

The live transcript remains usable immediately; refinement runs only after the
stream releases its GPU execution lease.

---

### `GET /v1/audio/transcription-jobs/{id}`

Poll the optional post-stream pass. States are `queued`, `running`,
`succeeded`, or `failed`. A succeeded response includes `text`, `language`,
and `duration`, along with `model` and the original `source_model`. These jobs
exist only when the WebSocket request explicitly supplied
`post_process_model`.

### `POST /v1/audio/analysis-jobs`

Queues an autodubbing analysis and returns `202` with `id`, `status_url`, and
`result_url`. This is multipart: `file` is required; optional fields are
`transcribe` (default true), `transcription_model`, `language`,
`include_speaker_embeddings` (default true), and `separate_dialogue` (default
false). Audio and video formats supported by FFmpeg are normalized
automatically.

The stages use the shared GPU lifecycle/VRAM budget. Dialogue separation,
diarization, and STT release the GPU execution lease between stages; WeSpeaker
embedding runs on CPU.

### `GET /v1/audio/analysis-jobs/{id}`

Returns `queued`, `running`, `succeeded`, or `failed` plus timestamps and the
result URL. Analysis is asynchronous and does not hold the upload connection.

### `GET /v1/audio/analysis-jobs/{id}/result`

Returns `202` until ready, then speaker-attributed timeline segments, raw
diarization turns, optional captions, and optional speaker embeddings. See
[Autodubbing analysis](autodubbing.md) for a complete example and limitations.

## Forced alignment

Word- and phone-level timings for audio you already have a transcript for.
Alignment runs Montreal Forced Aligner out of process and is asynchronous: the
`POST` returns `202` with a job ID, then you poll.

### `GET /v1/audio/alignments/models`

Lists the language codes that `mfa/models.yaml` maps to an acoustic model and
dictionary. A code appearing here does not guarantee the underlying MFA models
are downloaded — an aligner whose models are missing fails at job time.

### `POST /v1/audio/alignments`

```bash
ID=$(curl -sS http://127.0.0.1:8010/v1/audio/alignments \
  -F file=@speech.wav \
  -F 'transcript=forced alignment is working for english now' \
  -F language=en | jq -r .id)
```

**Form fields**

| Field | Required | Description |
|---|---|---|
| `file` | yes | Audio file. Converted to 16 kHz mono WAV before alignment. |
| `transcript` | yes | Plain-text transcript of the whole clip. |
| `language` | yes | A code returned by `/v1/audio/alignments/models`. |

### `GET /v1/audio/alignments/{id}`

Returns `queued`, `running`, `succeeded`, or `failed`. On success the `result`
holds `words` and `phones`, each with `start`/`end` seconds:

```json
{
  "status": "succeeded",
  "result": {
    "words": [ { "start": 0.32, "end": 0.70, "text": "forced" } ],
    "phones": [ { "start": 0.32, "end": 0.38, "phone": "f" } ]
  }
}
```

Alignment is CPU-bound and not fast: expect roughly ten seconds of wall clock
per second of audio on a cold model.

## Authentication

When `AUDIO_API_KEY` is set, all `/v1/` endpoints require one of:

- `Authorization: Bearer <key>` header
- `X-API-Key: <key>` header

The `/healthz` endpoint is always unauthenticated.
