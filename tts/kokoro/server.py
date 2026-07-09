#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing Kokoro TTS over HTTP.

The Go audio server's job queue owns scheduling and VRAM lifecycle
decisions; this sidecar just does what it's told: POST /load loads the
shared model, POST /unload frees it, POST /synthesize generates audio
(lazily loading first if needed), GET /voices and GET /health report state.

Kokoro (hexgrad/Kokoro-82M) is a single ~82M-parameter model shared across
languages; per-language behavior lives in a KPipeline (grapheme-to-phoneme
frontend), one of which is created lazily per requested language and cached.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
from typing import Any

import numpy as np
import soundfile as sf
import torch
from kokoro import KModel, KPipeline

DEFAULT_REPO_ID = "hexgrad/Kokoro-82M"
DEFAULT_LANGUAGE = "en-us"
DEFAULT_VOICE = "af_heart"
DEFAULT_SPEED = 1.0
SAMPLE_RATE = 24000

# Maps our BCP-47-ish language field to Kokoro's internal lang_code, mirroring
# kokoro.pipeline.ALIASES.
LANGUAGE_TO_LANG_CODE = {
    "en-us": "a",
    "en-gb": "b",
    "es": "e",
    "fr-fr": "f",
    "hi": "h",
    "it": "i",
    "pt-br": "p",
    "ja": "j",
    "zh": "z",
}

# A curated subset of the well-known public voice names for Kokoro-82M.
# Not exhaustive — see the model card on Hugging Face for the full list.
VOICES = [
    {"provider": "kokoro", "voice": "af_heart", "language": "en-us", "name": "American English (female)"},
    {"provider": "kokoro", "voice": "af_bella", "language": "en-us", "name": "American English (female)"},
    {"provider": "kokoro", "voice": "af_nicole", "language": "en-us", "name": "American English (female)"},
    {"provider": "kokoro", "voice": "am_adam", "language": "en-us", "name": "American English (male)"},
    {"provider": "kokoro", "voice": "am_michael", "language": "en-us", "name": "American English (male)"},
    {"provider": "kokoro", "voice": "bf_emma", "language": "en-gb", "name": "British English (female)"},
    {"provider": "kokoro", "voice": "bf_isabella", "language": "en-gb", "name": "British English (female)"},
    {"provider": "kokoro", "voice": "bm_george", "language": "en-gb", "name": "British English (male)"},
    {"provider": "kokoro", "voice": "bm_lewis", "language": "en-gb", "name": "British English (male)"},
]


def choose_device(requested: str) -> str:
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda"
    if hasattr(torch.backends, "mps") and torch.backends.mps.is_available():
        return "mps"
    return "cpu"


def normalize_language(language: str) -> str:
    return (language or DEFAULT_LANGUAGE).strip().lower().replace("_", "-")


class Engine:
    """Lazily loads a shared KModel and per-language KPipelines.

    The same lock guards load, unload, and generate so a load/unload can
    never race with an in-flight generation.
    """

    def __init__(self, repo_id: str, device: str) -> None:
        self.repo_id = repo_id
        self.device = choose_device(device)
        self.lock = threading.Lock()
        self.model: KModel | None = None
        self.pipelines: dict[str, KPipeline] = {}

    def is_loaded(self) -> bool:
        return self.model is not None

    def load(self) -> None:
        with self.lock:
            if self.model is not None:
                return
            print(f"loading {self.repo_id} on {self.device}...", file=sys.stderr)
            self.model = KModel(repo_id=self.repo_id).to(self.device).eval()
            print("model loaded", file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            if self.model is None:
                return
            self.pipelines.clear()
            del self.model
            self.model = None
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
            print("model unloaded", file=sys.stderr)

    def _pipeline_for(self, lang_code: str) -> KPipeline:
        pipeline = self.pipelines.get(lang_code)
        if pipeline is None:
            pipeline = KPipeline(lang_code=lang_code, model=self.model, device=self.device)
            self.pipelines[lang_code] = pipeline
        return pipeline

    def synthesize(self, text: str, language: str, voice: str, speed: float) -> bytes:
        self.load()
        lang_code = LANGUAGE_TO_LANG_CODE.get(normalize_language(language), "a")
        with self.lock:
            pipeline = self._pipeline_for(lang_code)
            chunks = []
            for result in pipeline(text, voice=voice, speed=speed):
                if result.audio is not None:
                    audio = result.audio
                    chunks.append(audio.numpy() if hasattr(audio, "numpy") else audio)
        audio = np.concatenate(chunks) if len(chunks) > 1 else chunks[0]
        buf = BytesIO()
        sf.write(buf, audio, SAMPLE_RATE, format="WAV", subtype="PCM_16")
        return buf.getvalue()


class Handler(BaseHTTPRequestHandler):
    engine: Engine  # set on the class before serving

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003
        print(f"{self.address_string()} - {fmt % args}", file=sys.stderr)

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_error_json(self, status: int, message: str) -> None:
        self._send_json(status, {"error": message})

    def do_GET(self) -> None:  # noqa: N802
        if self.path.rstrip("/") in ("", "/health"):
            self._send_json(200, {"status": "ok", "model_loaded": self.engine.is_loaded()})
            return
        if self.path.startswith("/voices"):
            self._send_json(200, {"voices": VOICES})
            return
        self._send_error_json(404, "not found")

    def do_POST(self) -> None:  # noqa: N802
        if self.path.startswith("/load"):
            try:
                self.engine.load()
            except torch.OutOfMemoryError:
                self._send_error_json(503, "GPU ran out of memory while loading the model")
                return
            except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
                self._send_error_json(500, str(exc))
                return
            self._send_json(200, {"status": "ok", "model_loaded": True})
            return

        if self.path.startswith("/unload"):
            self.engine.unload()
            self._send_json(200, {"status": "ok", "model_loaded": False})
            return

        if not self.path.startswith("/synthesize"):
            self._send_error_json(404, "not found")
            return

        length = int(self.headers.get("Content-Length", "0") or "0")
        if length <= 0:
            self._send_error_json(400, "missing request body")
            return
        try:
            payload = json.loads(self.rfile.read(length))
        except json.JSONDecodeError as exc:
            self._send_error_json(400, f"invalid JSON: {exc}")
            return

        text = str(payload.get("text", "")).strip()
        if not text:
            self._send_error_json(400, "text is required")
            return

        language = str(payload.get("language") or DEFAULT_LANGUAGE)
        voice = str(payload.get("voice") or DEFAULT_VOICE)
        speed = float(payload.get("speed") or DEFAULT_SPEED)

        if speed <= 0:
            self._send_error_json(400, "speed must be greater than 0")
            return

        try:
            wav_bytes = self.engine.synthesize(text=text, language=language, voice=voice, speed=speed)
        except torch.OutOfMemoryError:
            self._send_error_json(503, "GPU ran out of memory")
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav_bytes)))
        self.send_header("X-Sample-Rate", str(SAMPLE_RATE))
        self.end_headers()
        self.wfile.write(wav_bytes)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Kokoro TTS HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("KOKORO_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("KOKORO_PORT", "8021")))
    parser.add_argument("--repo-id", default=os.environ.get("KOKORO_REPO_ID", DEFAULT_REPO_ID))
    parser.add_argument("--device", default=os.environ.get("KOKORO_DEVICE", "auto"))
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("KOKORO_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first /load or /synthesize call",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    engine = Engine(args.repo_id, args.device)
    if args.preload:
        engine.load()
    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"kokoro sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
