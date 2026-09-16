#!/usr/bin/env python3
"""Sentence-segment stdin, print one sentence per line on stdout.

Reads a JSON object {"text": "...", "language": "en"} on stdin and writes
{"sentences": [...]} on stdout. Kept deliberately small: the caller does the
packing into synthesis chunks, so this process only answers "where does a
sentence end".

pysbd is a stopgap. See docs/TODO.md for the trained segmentation model that
should replace it; keep this stdin/stdout contract so the replacement is a
drop-in.
"""

import json
import sys

# pysbd ships 22 languages; anything else falls back to English rules, which is
# better than splitting on every period.
SUPPORTED = {
    "en", "hi", "mr", "zh", "es", "am", "ar", "hy", "bg", "ur", "ru", "pl",
    "fa", "nl", "da", "fr", "my", "el", "it", "ja", "de", "kk", "sk",
}


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        json.dump({"error": f"invalid request json: {exc}"}, sys.stdout)
        return 1

    text = payload.get("text") or ""
    if not text.strip():
        json.dump({"sentences": []}, sys.stdout)
        return 0

    language = (payload.get("language") or "en").strip().lower()
    # Accept BCP-47 ("en-US") by taking the primary subtag.
    language = language.split("-")[0]
    if language not in SUPPORTED:
        language = "en"

    try:
        import pysbd
    except ImportError as exc:
        json.dump({"error": f"pysbd is not installed: {exc}"}, sys.stdout)
        return 1

    try:
        segmenter = pysbd.Segmenter(language=language, clean=False)
        sentences = [s.strip() for s in segmenter.segment(text) if s.strip()]
    except Exception as exc:  # noqa: BLE001 - surface anything to the caller
        json.dump({"error": f"segmentation failed: {exc}"}, sys.stdout)
        return 1

    json.dump({"sentences": sentences}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
