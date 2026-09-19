#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing Chatterbox TTS voice cloning over HTTP.

The Go audio server's job queue owns scheduling and VRAM lifecycle
decisions; this sidecar just does what it's told: POST /load loads the
shared model, POST /unload frees it, POST /synthesize generates audio
(lazily loading first if needed) from a text prompt plus a local reference
WAV path, GET /health reports state.

Chatterbox (resemble-ai/chatterbox) clones from the audio prompt alone —
no matching transcript is required, unlike CosyVoice2's zero-shot mode.
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

import soundfile as sf
import torch
from chatterbox.tts import ChatterboxTTS

DEFAULT_DEVICE = "auto"


def choose_device(requested: str) -> str:
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda"
    if hasattr(torch.backends, "mps") and torch.backends.mps.is_available():
        return "mps"
    return "cpu"


class Engine:
    """Lazily loads a shared ChatterboxTTS model.

    The same lock guards load, unload, and generate so a load/unload can
    never race with an in-flight generation.
    """

    def __init__(self, device: str) -> None:
        self.device = choose_device(device)
        self.lock = threading.Lock()
        self.model: ChatterboxTTS | None = None

    def is_loaded(self) -> bool:
        return self.model is not None

    def load(self) -> None:
        with self.lock:
            if self.model is not None:
                return
            print(f"loading chatterbox on {self.device}...", file=sys.stderr)
            self.model = ChatterboxTTS.from_pretrained(device=self.device)
            print("model loaded", file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            if self.model is None:
                return
            del self.model
            self.model = None
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
            print("model unloaded", file=sys.stderr)

    def synthesize(
        self,
        text: str,
        reference_audio_path: str,
        exaggeration: float,
        cfg_weight: float,
        seed: int,
    ) -> bytes:
        self.load()
        if seed:
            torch.manual_seed(seed)
        with self.lock:
            wav = self.model.generate(
                text,
                audio_prompt_path=reference_audio_path,
                exaggeration=exaggeration,
                cfg_weight=cfg_weight,
            )
        samples = wav.squeeze(0).detach().cpu().numpy()
        buf = BytesIO()
        sf.write(buf, samples, self.model.sr, format="WAV", subtype="PCM_16")
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
        self._send_error_json(404, "not found")

    def do_POST(self) -> None:  # noqa: N802
        if self.path.startswith("/load"):
            try:
                self.engine.load()
            except torch.cuda.OutOfMemoryError:
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
        reference_audio_path = str(payload.get("reference_audio_path", "")).strip()
        if not reference_audio_path or not os.path.isfile(reference_audio_path):
            self._send_error_json(400, "reference_audio_path must point to an existing file")
            return
        exaggeration = float(payload.get("exaggeration") or 0.5)
        cfg_weight = float(payload.get("cfg_weight") or 0.5)
        seed = int(payload.get("seed") or 0)

        try:
            wav_bytes = self.engine.synthesize(text, reference_audio_path, exaggeration, cfg_weight, seed)
        except torch.cuda.OutOfMemoryError:
            self._send_error_json(503, "GPU ran out of memory")
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav_bytes)))
        self.end_headers()
        self.wfile.write(wav_bytes)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Chatterbox TTS HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("CHATTERBOX_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("CHATTERBOX_PORT", "8037")))
    parser.add_argument("--device", default=os.environ.get("CHATTERBOX_DEVICE", DEFAULT_DEVICE))
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("CHATTERBOX_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first /load or /synthesize call",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    engine = Engine(args.device)
    if args.preload:
        engine.load()
    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"chatterbox sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
