#!/usr/bin/env python3
"""Generate a WAV file from a UTF-8 Greek text file with OmniVoice."""

from __future__ import annotations

import argparse
import random
import sys
import threading
import time
from pathlib import Path
from types import TracebackType
from typing import TextIO

import numpy as np
import soundfile as sf
import torch
from omnivoice import OmniVoice


class ProgressBar:
    """Show activity and elapsed time for an operation with unknown progress."""

    def __init__(self, label: str, *, enabled: bool, stream: TextIO = sys.stderr):
        self.label = label
        self.enabled = enabled and stream.isatty()
        self.stream = stream
        self.started_at = 0.0
        self.stop_event = threading.Event()
        self.thread: threading.Thread | None = None

    @staticmethod
    def _elapsed(seconds: float) -> str:
        minutes, seconds = divmod(int(seconds), 60)
        return f"{minutes:02d}:{seconds:02d}"

    def _draw(self) -> None:
        width = 24
        marker = "====>"
        position = 0
        direction = 1
        while not self.stop_event.wait(0.1):
            cells = [" "] * width
            cells[position : position + len(marker)] = marker
            elapsed = self._elapsed(time.monotonic() - self.started_at)
            self.stream.write(f"\r{self.label} [{''.join(cells)}] {elapsed}")
            self.stream.flush()
            if position <= 0:
                direction = 1
            elif position >= width - len(marker):
                direction = -1
            position += direction

    def __enter__(self) -> ProgressBar:
        self.started_at = time.monotonic()
        if self.enabled:
            self.stream.write(f"{self.label} [{' ' * 24}] 00:00")
            self.stream.flush()
            self.thread = threading.Thread(target=self._draw, daemon=True)
            self.thread.start()
        else:
            print(f"{self.label}...", file=self.stream)
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self.stop_event.set()
        if self.thread is not None:
            self.thread.join()
        elapsed = self._elapsed(time.monotonic() - self.started_at)
        if self.enabled:
            status = "done" if exc_type is None else "failed"
            bar = "=" * 24 if exc_type is None else "!" * 24
            self.stream.write(f"\r{self.label} [{bar}] {elapsed} {status}\n")
            self.stream.flush()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Convert a UTF-8 Greek text file to speech with OmniVoice."
    )
    parser.add_argument("input", type=Path, help="UTF-8 text file containing Greek")
    parser.add_argument(
        "-o",
        "--output",
        type=Path,
        help="output WAV path (default: input filename with .wav extension)",
    )
    parser.add_argument(
        "--device",
        default="auto",
        help="inference device: auto, cuda:0, cpu, mps, or xpu (default: auto)",
    )
    parser.add_argument("--steps", type=int, default=32, help="diffusion steps")
    parser.add_argument("--speed", type=float, default=1.0, help="speech speed")
    parser.add_argument("--seed", type=int, default=42, help="random seed")
    parser.add_argument(
        "--chunk-seconds",
        type=float,
        default=12.0,
        help="target duration of chunks used for long text",
    )
    parser.add_argument(
        "--chunk-threshold",
        type=float,
        default=18.0,
        help="estimated duration at which long-text chunking starts",
    )
    parser.add_argument(
        "--model", default="k2-fsa/OmniVoice", help="Hugging Face model ID or path"
    )
    parser.add_argument(
        "--no-progress",
        action="store_true",
        help="disable the animated progress indicator",
    )
    return parser.parse_args()


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


def main() -> int:
    args = parse_args()

    try:
        text = args.input.read_text(encoding="utf-8-sig").strip()
    except OSError as exc:
        print(f"error: cannot read {args.input}: {exc}", file=sys.stderr)
        return 2

    if not text:
        print(f"error: {args.input} is empty", file=sys.stderr)
        return 2
    if args.steps < 1:
        print("error: --steps must be at least 1", file=sys.stderr)
        return 2
    if args.speed <= 0:
        print("error: --speed must be greater than 0", file=sys.stderr)
        return 2

    output = args.output or args.input.with_suffix(".wav")
    output.parent.mkdir(parents=True, exist_ok=True)

    device = choose_device(args.device)
    dtype = torch.float32 if device == "cpu" else torch.float16
    seed_everything(args.seed)

    show_progress = not args.no_progress
    with ProgressBar(
        f"[1/3] Loading {args.model} on {device}", enabled=show_progress
    ):
        model = OmniVoice.from_pretrained(args.model, device_map=device, dtype=dtype)

    try:
        with ProgressBar(
            f"[2/3] Generating Greek speech ({args.steps} diffusion steps)",
            enabled=show_progress,
        ):
            audio = model.generate(
                text=text,
                language="el",
                num_step=args.steps,
                speed=args.speed,
                audio_chunk_duration=args.chunk_seconds,
                audio_chunk_threshold=args.chunk_threshold,
            )[0]
    except torch.OutOfMemoryError:
        print(
            "error: GPU ran out of memory; retry with "
            "--chunk-seconds 8 --chunk-threshold 10 or --device cpu",
            file=sys.stderr,
        )
        return 1

    with ProgressBar(f"[3/3] Writing {output}", enabled=show_progress):
        sf.write(output, audio, model.sampling_rate, subtype="PCM_16")
    print(f"Wrote {output} ({len(audio) / model.sampling_rate:.1f}s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
