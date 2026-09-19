#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing a DnR-trained HTDemucs cinematic separator
(dialogue/music/effects) over HTTP, via the real PyTorch demucs package.

audio.cpp's native HTDemucs C++ port can't run this checkpoint — it has
bottom_channels=0 (the official "skip the bottleneck projection" mode),
which audio.cpp's implementation doesn't handle (see tmp/TOOD.md). Running
it through the real demucs package sidesteps that entirely.

The Go audio server's job queue owns scheduling and VRAM lifecycle
decisions; this sidecar just does what it's told: POST /load loads the
shared model, POST /unload frees it, POST /v1/tasks/run separates a clip
(lazily loading first if needed), GET /health reports state.

Checkpoint: ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing release v.1.0.0,
97d170e1-dbb4db15.th — a Demucs4/HTDemucs model trained on the DnR
(Divide and Remaster) dataset, https://github.com/ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing
"""

from __future__ import annotations

import argparse
import base64
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
from demucs.apply import apply_model
from demucs.states import load_model

DEFAULT_CHECKPOINT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "models", "97d170e1-dbb4db15.th")

# The checkpoint's own `sources` order (music, sfx, speech) mapped to the
# dialogue/music/effects names the rest of the pipeline expects.
STEM_NAMES = {"music": "music", "sfx": "effects", "speech": "dialogue"}


def choose_device(requested: str) -> str:
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda"
    return "cpu"


class Engine:
    """Lazily loads a shared HTDemucs model.

    The same lock guards load, unload, and separate so a load/unload can
    never race with an in-flight separation.
    """

    def __init__(self, checkpoint_path: str, device: str) -> None:
        self.checkpoint_path = checkpoint_path
        self.device = choose_device(device)
        self.lock = threading.Lock()
        self.model = None

    def is_loaded(self) -> bool:
        return self.model is not None

    def load(self) -> None:
        with self.lock:
            if self.model is not None:
                return
            print(f"loading {self.checkpoint_path} on {self.device}...", file=sys.stderr)
            model = load_model(self.checkpoint_path)
            model.to(self.device)
            model.eval()
            self.model = model
            print("model loaded, sources:", model.sources, file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            if self.model is None:
                return
            del self.model
            self.model = None
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
            print("model unloaded", file=sys.stderr)

    def separate(self, audio_path: str) -> dict[str, bytes]:
        self.load()
        wav, sample_rate = sf.read(audio_path, dtype="float32", always_2d=True)
        # wav: (frames, channels) -> (channels, frames)
        wav = wav.T
        if wav.shape[0] == 1:
            wav = np.concatenate([wav, wav], axis=0)
        mix = torch.from_numpy(wav).unsqueeze(0).to(self.device)

        with self.lock:
            with torch.no_grad():
                out = apply_model(self.model, mix, shifts=1, overlap=0.8, device=self.device)[0].cpu().numpy()

        stems: dict[str, bytes] = {}
        for i, source_name in enumerate(self.model.sources):
            stem_name = STEM_NAMES.get(source_name, source_name)
            samples = out[i].T  # (frames, channels)
            buf = BytesIO()
            sf.write(buf, samples, sample_rate, format="WAV", subtype="FLOAT")
            stems[stem_name] = buf.getvalue()
        return stems


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
        self._send_json(status, {"error": {"message": message}})

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

        if not self.path.startswith("/v1/tasks/run"):
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

        audio_path = str(payload.get("request", {}).get("audio", "")).strip()
        if not audio_path or not os.path.isfile(audio_path):
            self._send_error_json(400, "request.audio must point to an existing file")
            return

        try:
            stems = self.engine.separate(audio_path)
        except torch.cuda.OutOfMemoryError:
            self._send_error_json(503, "GPU ran out of memory")
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self._send_json(200, {
            "named_audio_outputs": [
                {"id": name, "audio": base64.b64encode(data).decode("ascii")}
                for name, data in stems.items()
            ]
        })


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="HTDemucs DnR cinematic separator HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("HTDEMUCS_DNR_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("HTDEMUCS_DNR_PORT", "8041")))
    parser.add_argument("--checkpoint", default=os.environ.get("HTDEMUCS_DNR_CHECKPOINT", DEFAULT_CHECKPOINT))
    parser.add_argument("--device", default=os.environ.get("HTDEMUCS_DNR_DEVICE", "auto"))
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("HTDEMUCS_DNR_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first /load or /v1/tasks/run call",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    engine = Engine(args.checkpoint, args.device)
    if args.preload:
        engine.load()
    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"htdemucs-dnr sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
