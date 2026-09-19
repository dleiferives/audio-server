#!/usr/bin/env python3
"""HTTP sidecar exposing Moonshine v2 streaming transcription over HTTP.

This is the CPU-only STT path, built on the `moonshine-voice` pip package
(Moonshine v2, released February 2026). Moonshine's streaming architecture
caches the encoder output and part of the decoder state, so it refines a
transcript as audio arrives instead of re-decoding a growing buffer — most of
the work happens while the speaker is still talking. That makes the live
endpoint the point of this provider, not an afterthought.

Do not confuse this with the GGUF `moonshine` model that audio.cpp serves on the
GPU. This sidecar runs entirely on the CPU and is not registered against the GPU
residency budget. See stt/moonshine/README.md.

Two HTTP surfaces, matching the conventions the Go providers already speak:

  * POST /transcribe            buffered: a 16 kHz mono PCM16 WAV body in, JSON out
  * POST /stream/begin          open the single live session
    POST /stream/feed           raw PCM16 frames in, an incremental update out
    POST /stream/finalize       flush and return the final transcript
    POST /stream/reset          discard the session

Like transcribe.cpp's sidecar, only one live session exists at a time; the Go
side serializes access to it.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
from typing import Any
from urllib.parse import parse_qs, urlparse

DEFAULT_LANGUAGE = "en"

# medium-streaming is the package default for English and, measured on CPU, is
# both faster and markedly more accurate than small-streaming. There is no
# "base" streaming arch.
DEFAULT_ARCH = "MEDIUM_STREAMING"

DEFAULT_UPDATE_INTERVAL = 0.5
DEFAULT_IDLE_UNLOAD_SECONDS = 300.0

SAMPLE_RATE = 16000


def configure_cpu_only(threads: int) -> None:
    """Pin inference to the CPU before the native library is imported.

    moonshine-voice ships libmoonshine.so over ONNX Runtime. Hiding the GPU
    here means this sidecar cannot claim VRAM even if a GPU-enabled runtime is
    present. Thread counts only take effect before the first import.
    """
    os.environ["CUDA_VISIBLE_DEVICES"] = ""
    if threads > 0:
        os.environ.setdefault("OMP_NUM_THREADS", str(threads))
        os.environ.setdefault("ORT_INTRA_OP_NUM_THREADS", str(threads))


def decode_wav(audio: bytes) -> tuple[list[float], float]:
    """Decode 16-bit mono 16 kHz PCM WAV into floats in [-1, 1].

    The Go provider declares AudioRequirements{wav, pcm_s16le, 16000, 1}, so the
    server normalizes every upload with ffmpeg before it arrives here.
    """
    with wave.open(BytesIO(audio), "rb") as handle:
        channels = handle.getnchannels()
        width = handle.getsampwidth()
        rate = handle.getframerate()
        frames = handle.readframes(handle.getnframes())

    if width != 2:
        raise ValueError(f"expected 16-bit PCM, got {width * 8}-bit")
    if channels != 1:
        raise ValueError(f"expected mono audio, got {channels} channels")
    if rate != SAMPLE_RATE:
        raise ValueError(f"expected {SAMPLE_RATE} Hz audio, got {rate} Hz")

    return pcm16_to_floats(frames), len(frames) / 2.0 / SAMPLE_RATE


def pcm16_to_floats(frames: bytes) -> list[float]:
    import numpy as np

    return (np.frombuffer(frames, dtype="<i2").astype(np.float32) / 32768.0).tolist()


def split_lines(transcript: Any) -> tuple[str, str]:
    """Return (committed, tentative) text for a transcript.

    A line Moonshine has finished with is committed and will not change; the
    trailing open line is still being revised as more audio arrives. Note that
    TranscriptLine also carries the line's raw audio in .audio_data — only the
    text is read here.
    """
    committed = " ".join(line.text.strip() for line in transcript.lines if line.is_complete and line.text.strip())
    tentative = " ".join(
        line.text.strip() for line in transcript.lines if not line.is_complete and line.text.strip()
    )
    return committed, tentative


def joined(transcript: Any) -> str:
    return " ".join(line.text.strip() for line in transcript.lines if line.text.strip())


class Engine:
    """Owns the Transcriber and the single live stream.

    Moonshine publishes one model per language, so switching language means
    loading a different model; only one is kept resident. The lock guards load,
    unload, and every transcription entry point, and the idle timer is reset on
    each request so an unused model is released.
    """

    def __init__(
        self,
        language: str,
        arch_name: str,
        update_interval: float,
        idle_unload_seconds: float,
        options: dict[str, str],
    ) -> None:
        self.language = language
        self.arch_name = arch_name
        self.update_interval = update_interval
        self.idle_unload_seconds = idle_unload_seconds
        self.options = options
        self.lock = threading.RLock()
        self.transcriber: Any | None = None
        self.loaded_language: str | None = None
        self.idle_timer: threading.Timer | None = None

        # Live session state.
        self.stream: Any | None = None
        self.input_samples = 0
        self.revision = 0
        self.last_text = ""

    def is_loaded(self) -> bool:
        return self.transcriber is not None

    def load(self, language: str | None = None) -> None:
        with self.lock:
            self._load_locked(language or self.language)

    def _load_locked(self, language: str) -> None:
        if self.transcriber is not None and self.loaded_language == language:
            return
        if self.transcriber is not None:
            # A different language was requested; release the resident model.
            self._unload_locked()

        import moonshine_voice as mv
        from moonshine_voice.transcriber import Transcriber

        supported = mv.supported_languages()
        if language not in supported:
            raise ValueError(f"unsupported language {language!r}; supported: {', '.join(sorted(supported))}")

        arch = getattr(mv.ModelArch, self.arch_name, None)
        if arch is None:
            raise ValueError(f"unknown model arch {self.arch_name!r}")

        print(f"loading moonshine language={language} arch={self.arch_name}...", file=sys.stderr)
        # Raises with the list of published archs if this language has no build
        # of the requested arch, which is the useful error to surface.
        model_path, resolved = mv.get_model_for_language(language, arch)
        self.transcriber = Transcriber(
            model_path=model_path,
            model_arch=resolved,
            update_interval=self.update_interval,
            options=self.options or None,
        )
        self.loaded_language = language
        print(f"model loaded ({mv.model_arch_to_string(resolved)})", file=sys.stderr)

    def unload(self) -> None:
        with self.lock:
            self._cancel_idle_timer_locked()
            self._unload_locked()

    def _unload_locked(self) -> None:
        self._reset_stream_locked()
        if self.transcriber is None:
            return
        try:
            self.transcriber.close()
        except Exception as exc:  # noqa: BLE001 - unloading must not take the sidecar down
            print(f"error closing transcriber: {exc}", file=sys.stderr)
        self.transcriber = None
        self.loaded_language = None
        print("model unloaded", file=sys.stderr)

    def _cancel_idle_timer_locked(self) -> None:
        if self.idle_timer is not None:
            self.idle_timer.cancel()
            self.idle_timer = None

    def _reset_idle_timer_locked(self) -> None:
        self._cancel_idle_timer_locked()
        # Never unload underneath an open live session.
        if self.idle_unload_seconds > 0 and self.stream is None:
            self.idle_timer = threading.Timer(self.idle_unload_seconds, self.unload)
            self.idle_timer.daemon = True
            self.idle_timer.start()

    # ── buffered ──

    def transcribe(self, audio: bytes, language: str | None) -> tuple[str, str, float]:
        samples, duration = decode_wav(audio)
        with self.lock:
            target = language or self.language
            self._load_locked(target)
            try:
                transcript = self.transcriber.transcribe_without_streaming(samples, SAMPLE_RATE)
            finally:
                self._reset_idle_timer_locked()
            return joined(transcript), target, duration

    # ── live ──

    def begin(self, language: str | None) -> None:
        with self.lock:
            self._cancel_idle_timer_locked()
            self._reset_stream_locked()
            self._load_locked(language or self.language)
            self.stream = self.transcriber.create_stream(update_interval=self.update_interval)
            self.stream.start()
            self.input_samples = 0
            self.revision = 0
            self.last_text = ""

    def feed(self, frames: bytes) -> dict[str, Any]:
        with self.lock:
            if self.stream is None:
                raise LookupError("no live session; POST /stream/begin first")
            samples = pcm16_to_floats(frames)
            self.stream.add_audio(samples, SAMPLE_RATE)
            self.input_samples += len(samples)
            return self._update_locked(final=False)

    def finalize(self) -> dict[str, Any]:
        with self.lock:
            if self.stream is None:
                raise LookupError("no live session; POST /stream/begin first")
            self.stream.stop()
            update = self._update_locked(final=True)
            self._reset_stream_locked()
            self._reset_idle_timer_locked()
            return update

    def _update_locked(self, final: bool) -> dict[str, Any]:
        transcript = self.stream.update_transcription()
        committed, tentative = split_lines(transcript)
        text = joined(transcript)

        changed = text != self.last_text
        if changed:
            self.revision += 1
            self.last_text = text

        input_ms = int(self.input_samples * 1000 / SAMPLE_RATE)
        # Audio behind the last committed line is still in flight.
        committed_ms = 0
        for line in transcript.lines:
            if line.is_complete:
                committed_ms = max(committed_ms, int((line.start_time + line.duration) * 1000))

        # Per-line timing lets the caller cut recorded audio into per-utterance
        # training clips on Moonshine's own boundaries.
        lines = [
            {
                "text": line.text.strip(),
                "start_ms": int(line.start_time * 1000),
                "duration_ms": int(line.duration * 1000),
                "complete": bool(line.is_complete),
            }
            for line in transcript.lines
            if line.text.strip()
        ]

        return {
            "text": text,
            "committed_text": committed,
            "tentative_text": tentative,
            "input_ms": input_ms,
            "buffered_ms": max(0, input_ms - committed_ms),
            "revision": self.revision,
            "changed": changed,
            "final": final,
            "lines": lines,
        }

    def reset(self) -> None:
        with self.lock:
            self._reset_stream_locked()
            self._reset_idle_timer_locked()

    def _reset_stream_locked(self) -> None:
        if self.stream is None:
            return
        try:
            self.stream.stop()
        except Exception:  # noqa: BLE001 - already-stopped streams are fine
            pass
        try:
            self.stream.close()
        except Exception as exc:  # noqa: BLE001 - reset must always succeed
            print(f"error closing stream: {exc}", file=sys.stderr)
        self.stream = None
        self.input_samples = 0
        self.revision = 0
        self.last_text = ""


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
        # Both shapes are sent: the buffered provider reads "error", the live
        # provider reads "error.message".
        self._send_json(status, {"error": {"message": message}, "message": message})

    def _requested_language(self) -> str | None:
        header = self.headers.get("X-Transcribe-Language", "")
        query = parse_qs(urlparse(self.path).query).get("language", [""])[0]
        return normalize_language(header or query)

    def do_GET(self) -> None:  # noqa: N802
        if self.path.rstrip("/") in ("", "/health"):
            self._send_json(200, {"status": "ok", "model_loaded": self.engine.is_loaded()})
            return
        self._send_error_json(404, "not found")

    def do_POST(self) -> None:  # noqa: N802
        route = urlparse(self.path).path.rstrip("/")
        length = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(length) if length > 0 else b""

        try:
            if route == "/load":
                self.engine.load(self._requested_language())
                self._send_json(200, {"status": "ok", "model_loaded": True})
                return

            if route == "/unload":
                self.engine.unload()
                self._send_json(200, {"status": "ok", "model_loaded": False})
                return

            if route == "/stream/begin":
                self.engine.begin(self._requested_language())
                self._send_json(200, {"status": "ok"})
                return

            if route == "/stream/feed":
                if not body or len(body) % 2 != 0:
                    self._send_error_json(400, "PCM frame must contain complete 16-bit samples")
                    return
                self._send_json(200, self.engine.feed(body))
                return

            if route == "/stream/finalize":
                self._send_json(200, self.engine.finalize())
                return

            if route == "/stream/reset":
                self.engine.reset()
                self._send_json(200, {"status": "ok"})
                return

            if route == "/transcribe":
                if not body:
                    self._send_error_json(400, "missing request body")
                    return
                text, language, duration = self.engine.transcribe(body, self._requested_language())
                self._send_json(200, {"text": text, "language": language, "duration": duration})
                return
        except LookupError as exc:
            self._send_error_json(409, str(exc))
            return
        except ValueError as exc:
            self._send_error_json(400, str(exc))
            return
        except Exception as exc:  # noqa: BLE001 - report to caller instead of crashing sidecar
            self._send_error_json(500, str(exc))
            return

        self._send_error_json(404, "not found")


def normalize_language(language: str) -> str | None:
    """Reduce a BCP-47 tag (en-US, el_GR) to the language subtag Moonshine uses."""
    language = language.strip().lower()
    if not language:
        return None
    for separator in ("-", "_"):
        if separator in language:
            language = language.split(separator, 1)[0]
    return language or None


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Moonshine v2 streaming HTTP sidecar (CPU only)")
    parser.add_argument("--host", default=os.environ.get("MOONSHINE_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("MOONSHINE_PORT", "8042")))
    parser.add_argument(
        "--language",
        default=os.environ.get("MOONSHINE_LANGUAGE", DEFAULT_LANGUAGE),
        help="default language; Moonshine ships one model per language",
    )
    parser.add_argument(
        "--model-arch",
        default=os.environ.get("MOONSHINE_MODEL_ARCH", DEFAULT_ARCH),
        help="ModelArch name, e.g. MEDIUM_STREAMING, SMALL_STREAMING, TINY_STREAMING",
    )
    parser.add_argument(
        "--update-interval",
        type=float,
        default=float(os.environ.get("MOONSHINE_UPDATE_INTERVAL", DEFAULT_UPDATE_INTERVAL)),
        help="seconds between streaming transcript refreshes",
    )
    parser.add_argument(
        "--threads",
        type=int,
        default=int(os.environ.get("MOONSHINE_THREADS", "0")),
        help="ONNX Runtime intra-op threads; 0 leaves the runtime default (all physical cores)",
    )
    parser.add_argument(
        "--idle-unload-seconds",
        type=float,
        default=float(os.environ.get("MOONSHINE_IDLE_UNLOAD_SECONDS", DEFAULT_IDLE_UNLOAD_SECONDS)),
        help="unload the model after this many idle seconds; 0 disables idle-unload",
    )
    parser.add_argument(
        "--option",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="advanced C API option passed through to Transcriber (repeatable), e.g. word_timestamps=true",
    )
    parser.add_argument(
        "--preload",
        action="store_true",
        default=os.environ.get("MOONSHINE_PRELOAD", "").lower() in ("1", "true", "yes"),
        help="load the model at startup instead of on first request",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()

    options: dict[str, str] = {}
    for item in args.option:
        if "=" not in item:
            print(f"--option expects KEY=VALUE, got {item!r}", file=sys.stderr)
            return 2
        key, value = item.split("=", 1)
        options[key.strip()] = value.strip()

    configure_cpu_only(args.threads)

    language = normalize_language(args.language) or DEFAULT_LANGUAGE
    engine = Engine(language, args.model_arch, args.update_interval, args.idle_unload_seconds, options)
    if args.preload:
        engine.load()

    Handler.engine = engine
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"moonshine sidecar listening on http://{args.host}:{args.port}", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        engine.unload()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
