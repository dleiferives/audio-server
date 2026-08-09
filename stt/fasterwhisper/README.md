# faster-whisper sidecar

`server.py` is a minimal HTTP sidecar around
[faster-whisper](https://github.com/SYSTRAN/faster-whisper), a CTranslate2
reimplementation of OpenAI's Whisper. See
[`docs/providers.md`](../../docs/providers.md#faster-whisper) for the full
contract and shared GPU lifecycle behavior.

Install in a Python 3.10+ environment:

```bash
pip install faster-whisper torch
```

Run the sidecar:

```bash
./server.py --port 8030 --model-size large-v3 --device cuda --compute-type int8
```

`--model-size` accepts any faster-whisper model size (`tiny`, `base`,
`small`, `medium`, `large-v3`, ...) — bigger models are more accurate but
slower and use more VRAM. `--device auto` (default) picks CUDA if available,
otherwise falls back to CPU.

When launched by `audio-server`, the sidecar shares an exclusive lifecycle
group with the audio.cpp GPU providers. The manager stops the current GPU
sidecar before starting this process and disables its internal idle timer.
When run independently, the model loads lazily and unloads after 60 idle
seconds by default; pass `--preload` to load it eagerly instead.
