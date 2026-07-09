# faster-whisper sidecar

`server.py` is a minimal HTTP sidecar around
[faster-whisper](https://github.com/SYSTRAN/faster-whisper), a CTranslate2
reimplementation of OpenAI's Whisper. See
[`docs/providers.md`](../../docs/providers.md#faster-whisper) for the full
contract and how it differs from the TTS sidecars (no Go-orchestrated job
queue — it manages its own idle-unload timer).

Install in a Python 3.10+ environment:

```bash
pip install faster-whisper torch
```

Run the sidecar:

```bash
./server.py --port 8030 --model-size small
```

`--model-size` accepts any faster-whisper model size (`tiny`, `base`,
`small`, `medium`, `large-v3`, ...) — bigger models are more accurate but
slower and use more VRAM. `--device auto` (default) picks CUDA if available,
otherwise falls back to CPU.

The model loads lazily on the first `/transcribe` (or `/load`) call and
unloads after `--idle-unload-seconds` (default 60) of inactivity; pass
`--preload` to load it eagerly at startup instead.
