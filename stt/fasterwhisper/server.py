#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing faster-whisper transcription over HTTP.

Unlike the TTS sidecars (tts/omnivoice, tts/kokoro), there's no Go-side job
queue driving this one yet — STT requests are typically single quick
round trips rather than slow GPU-bound generation, so scheduling doesn't
need the same "see queue position" treatment. To still respect a limited
VRAM budget, this sidecar manages its own idle-unload timer internally
(reset on every request) instead of waiting for a Go-orchestrated
Warm/Idle call. POST /load and POST /unload are still exposed so a future
queue-based integration can drive it explicitly, same shape as the other
sidecars.
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
from urllib.parse import parse_qs, urlparse

from faster_whisper import WhisperModel

DEFAULT_MODEL_SIZE = "small"
DEFAULT_IDLE_UNLOAD_SECONDS = 60.0


class Engine:
    """Lazily loads a WhisperModel and unloads it after an idle period.

    The lock guards load, unload, and transcribe so they can't race each
    other; the idle timer is reset on every request and only fires unload()
    if nothing else has happened in the meantime.
    """

    def __init__(self, model_size: str, device: str, compute_type: str, idle_unload_seconds: float) -> None:
        self.model_size = model_size
        self.device = device
        self.compute_type = compute_type
        self.idle_unload_seconds = idle_unload_seconds
        self.lock = threading.Lock()
        self.model: WhisperModel | None = None
        self.idle_timer: threading.Timer | None = None

    def is_loaded(self) -> bool:
        return self.model is not None

    def load(self) -> None:
        with self.lock:
            self._load_locked()

    def _load_locked(self) -> None:
        if self.model is not None:
            return
        print(f"loading whisper model={self.model_size} device={self.device}...", file=sys.stderr)
        self.model = WhisperModel(self.model_size, device=self.device, compute_type=self.compute_type)
        print("model loaded", file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            self._cancel_idle_timer_locked()
            if self.model is None:
                return
            del self.model
            self.model = None
            try:
                import torch

                if torch.cuda.is_available():
                    torch.cuda.empty_cache()
            except ImportError:
                pass
            print("model unloaded", file=sys.stderr)

    def _cancel_idle_timer_locked(self) -> None:
        if self.idle_timer is not None:
            self.idle_timer.cancel()
            self.idle_timer = None

    def _reset_idle_timer_locked(self) -> None:
        self._cancel_idle_timer_locked()
        if self.idle_unload_seconds > 0:
            self.idle_timer = threading.Timer(self.idle_unload_seconds, self.unload)
            self.idle_timer.daemon = True
            self.idle_timer.start()

    def transcribe(self, audio: bytes, language: str | None) -> tuple[str, str, float]:
        with self.lock:
            self._load_locked()
            segments, info = self.model.transcribe(BytesIO(audio), language=language or None)
            text = "".join(segment.text for segment in segments).strip()
            self._reset_idle_timer_locked()
        return text, info.language, info.duration


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
        self._send_error_json(404, "not found")

    def do_POST(self) -> None:  # noqa: N802
        if self.path.startswith("/load"):
            try:
                self.engine.load()
            except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
                self._send_error_json(500, str(exc))
                return
            self._send_json(200, {"status": "ok", "model_loaded": True})
            return

        if self.path.startswith("/unload"):
            self.engine.unload()
            self._send_json(200, {"status": "ok", "model_loaded": False})
            return

        if not self.path.startswith("/transcribe"):
            self._send_error_json(404, "not found")
            return

        length = int(self.headers.get("Content-Length", "0") or "0")
        if length <= 0:
            self._send_error_json(400, "missing request body")
            return
        audio = self.rfile.read(length)

        query = parse_qs(urlparse(self.path).query)
        language = (query.get("language") or [""])[0].strip() or None

        try:
            text, detected_language, duration = self.engine.transcribe(audio, language)
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self._send_json(200, {"text": text, "language": detected_language, "duration": duration})


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="faster-whisper HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("FASTERWHISPER_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("FASTERWHISPER_PORT", "8030")))
    parser.add_argument("--model-size", default=os.environ.get("FASTERWHISPER_MODEL_SIZE", DEFAULT_MODEL_SIZE))
    parser.add_argument("--device", default=os.environ.get("FASTERWHISPER_DEVICE", "auto"))
    parser.add_argument("--compute-type", default=os.environ.get("FASTERWHISPER_COMPUTE_TYPE", "default"))
    parser.add_argument(
        "--idle-unload-seconds",
        type=float,
        default=float(os.environ.get("FASTERWHISPER_IDLE_UNLOAD_SECONDS", DEFAULT_IDLE_UNLOAD_SECONDS)),
        help="unload the model after this many idle seconds; 0 disables idle-unload",
    )
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("FASTERWHISPER_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first /load or /transcribe call",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    engine = Engine(args.model_size, args.device, args.compute_type, args.idle_unload_seconds)
    if args.preload:
        engine.load()
    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"faster-whisper sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
