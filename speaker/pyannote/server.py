#!/usr/bin/env python3
"""Minimal HTTP sidecar exposing pyannote.audio speaker diarization over HTTP.

The Go audio server's job queue owns scheduling and VRAM lifecycle
decisions; this sidecar just does what it's told: POST /load loads the
shared pipeline, POST /unload frees it, POST /diarize runs diarization plus
per-speaker embeddings on a whole clip in one pass (lazily loading first if
needed), GET /health reports state.

Unlike Sortformer (a fixed-graph model capped at a short window per
request), pyannote's community-1 pipeline segments, clusters, and embeds
speakers across an arbitrary-length clip in one call, and its embeddings can
stand in for a separate speaker-embedding stage (WeSpeaker) for callers that
select this provider.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import soundfile as sf
import torch
from pyannote.audio import Pipeline

DEFAULT_MODEL_ID = "pyannote/speaker-diarization-community-1"


def choose_device(requested: str) -> str:
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda"
    if hasattr(torch.backends, "mps") and torch.backends.mps.is_available():
        return "mps"
    return "cpu"


class Engine:
    """Lazily loads a shared diarization Pipeline.

    The same lock guards load, unload, and diarize so a load/unload can
    never race with an in-flight diarization.
    """

    def __init__(self, model_id: str, device: str, hf_token: str | None) -> None:
        self.model_id = model_id
        self.device = choose_device(device)
        self.hf_token = hf_token
        self.lock = threading.Lock()
        self.pipeline: Pipeline | None = None

    def is_loaded(self) -> bool:
        return self.pipeline is not None

    def load(self) -> None:
        with self.lock:
            if self.pipeline is not None:
                return
            print(f"loading {self.model_id} on {self.device}...", file=sys.stderr)
            pipeline = Pipeline.from_pretrained(self.model_id, token=self.hf_token)
            self.pipeline = pipeline.to(torch.device(self.device))
            print("pipeline loaded", file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            if self.pipeline is None:
                return
            del self.pipeline
            self.pipeline = None
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
            print("pipeline unloaded", file=sys.stderr)

    def diarize(
        self,
        wav_bytes: bytes,
        num_speakers: int | None = None,
        min_speakers: int | None = None,
        max_speakers: int | None = None,
    ) -> dict[str, Any]:
        self.load()
        with tempfile.NamedTemporaryFile(suffix=".wav") as tmp:
            tmp.write(wav_bytes)
            tmp.flush()
            info = sf.info(tmp.name)
            sample_rate = info.samplerate
            with self.lock:
                output = self.pipeline(
                    tmp.name,
                    num_speakers=num_speakers,
                    min_speakers=min_speakers,
                    max_speakers=max_speakers,
                )
            # pyannote.audio 4.x's SpeakerDiarization.apply returns a
            # DiarizeOutput with .speaker_diarization (an Annotation that
            # keeps overlapping turns) and .speaker_embeddings (an
            # (num_speakers, dim) array aligned to speaker_diarization.labels()).
            diarization = output.speaker_diarization
            embeddings = output.speaker_embeddings

        turns = []
        for segment, _, speaker in diarization.itertracks(yield_label=True):
            start_sample = max(0, round(segment.start * sample_rate))
            end_sample = max(start_sample, round(segment.end * sample_rate))
            if end_sample == start_sample:
                continue
            turns.append(
                {
                    "start_sample": start_sample,
                    "end_sample": end_sample,
                    "speaker_id": str(speaker),
                    "confidence": 1.0,
                }
            )

        speakers: dict[str, Any] = {}
        if embeddings is not None:
            for speaker, vector in zip(diarization.labels(), embeddings):
                values = vector.tolist()
                norm = sum(v * v for v in values) ** 0.5
                if norm > 0:
                    values = [v / norm for v in values]
                speakers[str(speaker)] = {"embedding": values, "dimensions": len(values)}

        return {
            "model": self.model_id,
            "duration": info.frames / sample_rate if sample_rate else 0.0,
            "sample_rate": sample_rate,
            "turns": turns,
            "speakers": speakers,
        }


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

        if not self.path.startswith("/diarize"):
            self._send_error_json(404, "not found")
            return

        length = int(self.headers.get("Content-Length", "0") or "0")
        if length <= 0:
            self._send_error_json(400, "missing request body")
            return
        wav_bytes = self.rfile.read(length)

        query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)

        def _int_param(name: str) -> int | None:
            values = query.get(name)
            if not values or not values[0]:
                return None
            try:
                value = int(values[0])
            except ValueError:
                return None
            return value if value > 0 else None

        try:
            result = self.engine.diarize(
                wav_bytes,
                num_speakers=_int_param("num_speakers"),
                min_speakers=_int_param("min_speakers"),
                max_speakers=_int_param("max_speakers"),
            )
        except torch.cuda.OutOfMemoryError:
            self._send_error_json(503, "GPU ran out of memory")
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self._send_json(200, result)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="pyannote.audio diarization HTTP sidecar")
    parser.add_argument("--host", default=os.environ.get("PYANNOTE_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("PYANNOTE_PORT", "8036")))
    parser.add_argument("--model-id", default=os.environ.get("PYANNOTE_MODEL", DEFAULT_MODEL_ID))
    parser.add_argument("--device", default=os.environ.get("PYANNOTE_DEVICE", "auto"))
    parser.add_argument("--hf-token", default=os.environ.get("HF_TOKEN"))
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("PYANNOTE_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first /load or /diarize call",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if not args.hf_token:
        print("warning: no HF_TOKEN set; gated pyannote models will fail to download", file=sys.stderr)
    engine = Engine(args.model_id, args.device, args.hf_token)
    if args.preload:
        engine.load()
    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"pyannote sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
