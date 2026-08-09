#!/usr/bin/env python3
"""Generate a Greek Supertonic corpus and benchmark faster-whisper on it."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import threading
import time
import unicodedata
import urllib.error
import urllib.request
import uuid
import wave
from pathlib import Path
from typing import Any


def request(url: str, body: bytes, content_type: str) -> tuple[bytes, dict[str, str]]:
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", content_type)
    try:
        with urllib.request.urlopen(req) as response:
            return response.read(), dict(response.headers.items())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")
        raise RuntimeError(f"HTTP {exc.code}: {detail}") from exc


def multipart(fields: dict[str, str], filename: str, audio: bytes) -> tuple[bytes, str]:
    boundary = f"----audio-server-benchmark-{uuid.uuid4().hex}"
    chunks: list[bytes] = []
    for name, value in fields.items():
        chunks.extend(
            [
                f"--{boundary}\r\n".encode(),
                f'Content-Disposition: form-data; name="{name}"\r\n\r\n'.encode(),
                value.encode("utf-8"),
                b"\r\n",
            ]
        )
    chunks.extend(
        [
            f"--{boundary}\r\n".encode(),
            f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'.encode(),
            b"Content-Type: audio/wav\r\n\r\n",
            audio,
            b"\r\n",
            f"--{boundary}--\r\n".encode(),
        ]
    )
    return b"".join(chunks), f"multipart/form-data; boundary={boundary}"


def wav_duration(path: Path) -> float:
    with wave.open(str(path), "rb") as wav:
        return wav.getnframes() / wav.getframerate()


def resample_16k_mono(paths: list[Path], output_dir: Path) -> list[Path]:
    converted_dir = output_dir / "16k-mono"
    converted_dir.mkdir(parents=True, exist_ok=True)
    converted: list[Path] = []
    for source in paths:
        destination = converted_dir / source.name
        subprocess.run(
            [
                "ffmpeg",
                "-hide_banner",
                "-loglevel",
                "error",
                "-y",
                "-i",
                str(source),
                "-ac",
                "1",
                "-ar",
                "16000",
                str(destination),
            ],
            check=True,
        )
        converted.append(destination)
    return converted


def words(text: str, strip_marks: bool = False) -> list[str]:
    text = unicodedata.normalize("NFD" if strip_marks else "NFC", text.lower())
    if strip_marks:
        text = "".join(char for char in text if unicodedata.category(char) != "Mn")
    return re.findall(r"[^\W\d_]+", text, flags=re.UNICODE)


def edit_distance(reference: list[str], hypothesis: list[str]) -> int:
    row = list(range(len(hypothesis) + 1))
    for i, ref_item in enumerate(reference, 1):
        next_row = [i]
        for j, hyp_item in enumerate(hypothesis, 1):
            next_row.append(
                min(
                    next_row[-1] + 1,
                    row[j] + 1,
                    row[j - 1] + (ref_item != hyp_item),
                )
            )
        row = next_row
    return row[-1]


def error_rate(reference: list[str], hypothesis: list[str]) -> float:
    return edit_distance(reference, hypothesis) / max(1, len(reference))


class GPUSampler:
    def __init__(self) -> None:
        self.stop_event = threading.Event()
        self.samples: list[int] = []
        self.thread = threading.Thread(target=self._run, daemon=True)

    def _run(self) -> None:
        while not self.stop_event.is_set():
            try:
                output = subprocess.check_output(
                    [
                        "nvidia-smi",
                        "--query-gpu=memory.used",
                        "--format=csv,noheader,nounits",
                    ],
                    text=True,
                    stderr=subprocess.DEVNULL,
                    timeout=2,
                )
                self.samples.append(int(output.splitlines()[0].strip()))
            except (OSError, ValueError, subprocess.SubprocessError, IndexError):
                pass
            self.stop_event.wait(0.1)

    def __enter__(self) -> "GPUSampler":
        self.thread.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.stop_event.set()
        self.thread.join(timeout=2)

    @property
    def peak_mib(self) -> int | None:
        return max(self.samples) if self.samples else None


def generate(endpoint: str, output_dir: Path, item: dict[str, str]) -> Path:
    path = output_dir / f"{item['id']}.wav"
    payload = json.dumps(
        {
            "model": "supertonic",
            "input": item["text"],
            "voice": item["voice"],
            "language": "el",
            "response_format": "wav",
            "speed": 1,
        },
        ensure_ascii=False,
    ).encode("utf-8")
    audio, _ = request(f"{endpoint}/v1/audio/speech", payload, "application/json")
    path.write_bytes(audio)
    return path


def transcribe(endpoint: str, path: Path, stt_model: str, language: str) -> tuple[str, float, int | None]:
    body, content_type = multipart(
        {
            "model": stt_model,
            "language": language,
            "response_format": "json",
        },
        path.name,
        path.read_bytes(),
    )
    with GPUSampler() as gpu:
        started = time.perf_counter()
        response, _ = request(f"{endpoint}/v1/audio/transcriptions", body, content_type)
        elapsed = time.perf_counter() - started
    result = json.loads(response)
    return result["text"], elapsed, gpu.peak_mib


def benchmark(
    endpoint: str,
    corpus_path: Path,
    output_dir: Path,
    stt_model: str,
    language: str,
    reuse_audio: bool,
    convert_16k_mono: bool,
) -> dict[str, Any]:
    corpus: list[dict[str, str]] = json.loads(corpus_path.read_text())
    output_dir.mkdir(parents=True, exist_ok=True)

    audio_paths: list[Path] = []
    if reuse_audio:
        print("Reusing existing Greek Supertonic audio...", flush=True)
        for item in corpus:
            path = output_dir / f"{item['id']}.wav"
            if not path.is_file():
                raise FileNotFoundError(f"missing audio fixture: {path}")
            audio_paths.append(path)
    else:
        print("Generating Greek audio with Supertonic...", flush=True)
        for item in corpus:
            print(f"  {item['id']} ({item['voice']})", flush=True)
            audio_paths.append(generate(endpoint, output_dir, item))

    if convert_16k_mono:
        print("Converting benchmark input to mono 16 kHz...", flush=True)
        audio_paths = resample_16k_mono(audio_paths, output_dir)

    print(f"Transcribing with {stt_model}...", flush=True)
    rows: list[dict[str, Any]] = []
    for index, (item, path) in enumerate(zip(corpus, audio_paths)):
        duration = wav_duration(path)
        hypothesis, elapsed, peak_mib = transcribe(endpoint, path, stt_model, language)
        ref_words = words(item["text"])
        hyp_words = words(hypothesis)
        ref_plain = words(item["text"], strip_marks=True)
        hyp_plain = words(hypothesis, strip_marks=True)
        row = {
            "id": item["id"],
            "voice": item["voice"],
            "cold_start": index == 0,
            "audio_seconds": duration,
            "elapsed_seconds": elapsed,
            "real_time_factor": elapsed / duration,
            "times_realtime": duration / elapsed,
            "peak_gpu_mib": peak_mib,
            "wer": error_rate(ref_words, hyp_words),
            "wer_accent_insensitive": error_rate(ref_plain, hyp_plain),
            "cer": error_rate(list("".join(ref_words)), list("".join(hyp_words))),
            "reference": item["text"],
            "transcript": hypothesis,
            "audio_file": str(path),
        }
        rows.append(row)
        print(
            f"  {item['id']}: {duration:.2f}s audio in {elapsed:.2f}s, "
            f"{row['times_realtime']:.2f}x realtime, WER {row['wer']:.1%}",
            flush=True,
        )

    total_audio = sum(row["audio_seconds"] for row in rows)
    total_elapsed = sum(row["elapsed_seconds"] for row in rows)
    total_ref_words = sum(len(words(item["text"])) for item in corpus)
    total_word_errors = sum(
        edit_distance(words(item["text"]), words(row["transcript"]))
        for item, row in zip(corpus, rows)
    )
    warm = rows[1:] or rows
    aggregate = {
        "files": len(rows),
        "total_audio_seconds": total_audio,
        "total_elapsed_seconds": total_elapsed,
        "overall_times_realtime_including_cold_start": total_audio / total_elapsed,
        "warm_times_realtime": sum(row["audio_seconds"] for row in warm)
        / sum(row["elapsed_seconds"] for row in warm),
        "micro_average_wer": total_word_errors / max(1, total_ref_words),
        "peak_gpu_mib": max(
            (row["peak_gpu_mib"] for row in rows if row["peak_gpu_mib"] is not None),
            default=None,
        ),
    }
    return {
        "configuration": {
            "tts": "supertonic",
            "stt": stt_model,
            "language": language,
            "input_audio": "mono 16 kHz" if convert_16k_mono else "Supertonic source WAV",
        },
        "aggregate": aggregate,
        "files": rows,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--endpoint", default="http://127.0.0.1:8010")
    parser.add_argument("--corpus", type=Path, default=Path("benchmarks/greek-supertonic/corpus.json"))
    parser.add_argument("--output-dir", type=Path, default=Path("benchmarks/greek-supertonic/audio"))
    parser.add_argument("--results", type=Path, default=Path("benchmarks/greek-supertonic/results.json"))
    parser.add_argument("--stt-model", default="faster-whisper")
    parser.add_argument("--language", default="el")
    parser.add_argument("--reuse-audio", action="store_true")
    parser.add_argument("--convert-16k-mono", action="store_true")
    args = parser.parse_args()

    results = benchmark(
        args.endpoint.rstrip("/"),
        args.corpus,
        args.output_dir,
        args.stt_model,
        args.language,
        args.reuse_audio,
        args.convert_16k_mono,
    )
    args.results.write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(results["aggregate"], indent=2))
    print(f"Results written to {args.results}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
