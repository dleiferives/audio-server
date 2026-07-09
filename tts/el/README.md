# Greek text to speech

This script reads a UTF-8 Greek text file and creates a WAV using OmniVoice's
automatic voice. Long input is chunked internally while retaining the first
generated voice across subsequent chunks.

Install OmniVoice in a Python 3.10+ environment with CUDA-enabled PyTorch:

```bash
python -m pip install torch==2.8.0+cu128 torchaudio==2.8.0+cu128 \
  --extra-index-url https://download.pytorch.org/whl/cu128
python -m pip install omnivoice==0.1.5
```

Generate audio:

```bash
./el/greek_tts.py path/to/greek.txt
```

The default output is `path/to/greek.wav`. To choose another path:

```bash
./el/greek_tts.py path/to/greek.txt -o output.wav
```

Use `--steps 16` for faster generation or `--speed 1.1` for faster speech.
An animated indicator shows the current loading, generation, and writing stage;
pass `--no-progress` to disable the animation.
