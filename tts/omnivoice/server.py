#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing OmniVoice over POST /synthesize and GET /voices.

The Go audio server talks to this process over HTTP instead of shelling out
to a script per request, so the (slow) model load happens once at startup.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
from typing import Any

import numpy as np
import soundfile as sf
import torch
from omnivoice import OmniVoice

DEFAULT_MODEL_ID = "k2-fsa/OmniVoice"
DEFAULT_LANGUAGE = "el"
DEFAULT_STEPS = 32
DEFAULT_CHUNK_SECONDS = 12.0
DEFAULT_CHUNK_THRESHOLD = 18.0

VOICES = [
    {"provider": "omnivoice", "voice": "auto", "language": DEFAULT_LANGUAGE, "name": "OmniVoice automatic"},
]


def choose_device(requested: str) -> str:
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda:0"
    if hasattr(torch.backends, "mps") and torch.backends.mps.is_available():
        return "mps"
    if hasattr(torch, "xpu") and torch.xpu.is_available():
        return "xpu"
    return "cpu"


def seed_everything(seed: int) -> None:
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)
    if torch.cuda.is_available():
        torch.cuda.manual_seed_all(seed)


class Engine:
    """Loads the OmniVoice model once and serializes access to it.

    The underlying diffusion model is not known to be safe for concurrent
    calls from multiple threads, so a lock serializes generation.
    """

    def __init__(self, model_id: str, device: str) -> None:
        self.device = choose_device(device)
        self.dtype = torch.float32 if self.device == "cpu" else torch.float16
        self.lock = threading.Lock()
        print(f"loading {model_id} on {self.device}...", file=sys.stderr)
        self.model = OmniVoice.from_pretrained(model_id, device_map=self.device, dtype=self.dtype)
        print("model loaded", file=sys.stderr)

    def synthesize(self, text: str, language: str, steps: int, speed: float, seed: int,
                   chunk_seconds: float, chunk_threshold: float) -> tuple[bytes, int]:
        seed_everything(seed)
        with self.lock:
            audio = self.model.generate(
                text=text,
                language=language,
                num_step=steps,
                speed=speed,
                audio_chunk_duration=chunk_seconds,
                audio_chunk_threshold=chunk_threshold,
            )[0]
        buf = BytesIO()
        sf.write(buf, audio, self.model.sampling_rate, format="WAV", subtype="PCM_16")
        return buf.getvalue(), self.model.sampling_rate


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
            self._send_json(200, {"status": "ok"})
            return
        if self.path.startswith("/voices"):
            self._send_json(200, {"voices": VOICES})
            return
        self._send_error_json(404, "not found")

    def do_POST(self) -> None:  # noqa: N802
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
        steps = int(payload.get("steps") or DEFAULT_STEPS)
        speed = float(payload.get("speed") or 1.0)
        seed = int(payload.get("seed") or 42)
        chunk_seconds = float(payload.get("chunk_seconds") or DEFAULT_CHUNK_SECONDS)
        chunk_threshold = float(payload.get("chunk_threshold") or DEFAULT_CHUNK_THRESHOLD)

        if steps < 1:
            self._send_error_json(400, "steps must be at least 1")
            return
        if speed <= 0:
            self._send_error_json(400, "speed must be greater than 0")
            return

        try:
            wav_bytes, sample_rate = self.engine.synthesize(
                text=text,
                language=language,
                steps=steps,
                speed=speed,
                seed=seed,
                chunk_seconds=chunk_seconds,
                chunk_threshold=chunk_threshold,
            )
        except torch.OutOfMemoryError:
            self._send_error_json(
                503,
                "GPU ran out of memory; retry with smaller chunk_seconds/chunk_threshold",
            )
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav_bytes)))
        self.send_header("X-Sample-Rate", str(sample_rate))
        self.end_headers()
        self.wfile.write(wav_bytes)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="OmniVoice HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("OMNIVOICE_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("OMNIVOICE_PORT", "8020")))
    parser.add_argument("--model", default=os.environ.get("OMNIVOICE_MODEL", DEFAULT_MODEL_ID))
    parser.add_argument("--device", default=os.environ.get("OMNIVOICE_DEVICE", "auto"))
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    Handler.engine = Engine(args.model, args.device)
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"omnivoice sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
