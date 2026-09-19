# TODO

Longer-term work that is deliberately not being done yet. Each entry should say
what the current stopgap is, so the replacement has something to beat.

## Train a BERT-level segmentation model

**Status:** not started. Current stopgap: `pysbd`, a rules-based sentence
boundary disambiguator, behind an `auto_segment` option that is on by default.

Long input has to be split before synthesis or peak VRAM grows with input
length — a 256-word request tried to allocate a single 2004 MiB buffer and died
on a 4 GB card. Splitting on sentence boundaries is the natural cut point
because it is also where prosody resets, so a bad split is audible, not just
inefficient.

Rules-based segmentation is a stopgap. It is tuned for written prose and
mis-handles exactly the text people feed a TTS engine: dialogue with interleaved
attribution, ellipses and em-dashes used as prosodic pauses, lists and headings
with no terminal punctuation, transcripts with no punctuation at all, and
code-switched or unpunctuated non-English input. Every one of those produces a
chunk boundary in the wrong place, which the listener hears as a breath in the
middle of a clause.

The goal is a small encoder (BERT-base or smaller, ideally distilled to
something that fits alongside a TTS model on a 4 GB card) that does token-level
boundary prediction, trained on text paired with *where a speaker actually
pauses* rather than where an editor puts a period. Forced alignment already in
this repo can supply that signal: align a corpus, read the inter-word silences,
and treat pauses above a threshold as boundary labels. That turns alignment
output into segmentation training data for free.

Notes:

- [`wtpsplit`](https://github.com/segment-any-text/wtpsplit) (SaT / WtP) is the
  closest existing work and is the baseline to beat. It is SOTA multilingual but
  ~450x slower than rules-based on CPU, which is why it is not the stopgap.
- Evaluate on held-out *spoken* text, not the English Golden Rule Set. GRS is a
  small set that libraries overfit; it rewards newswire punctuation conventions
  that TTS input does not follow.
- Keep the segmenter a separate service from the TTS sidecar. The stopgap and
  the trained model should be swappable behind one interface, and the segmenter
  should not be resident on the GPU while a TTS model needs the VRAM.
- The metric that matters is not boundary F1, it is whether peak VRAM stays
  bounded and no chunk boundary lands mid-clause. Measure both.

## Re-transcribe the corpus with a stronger model, then train a corrector

**Status:** not started. Current stopgap: `stt_capture_enabled` records each clip
with a single `text` field holding whatever the live provider (Moonshine v2
`MEDIUM_STREAMING`) emitted, and nothing re-reads it.

Two separate goals share the same data. The first is a better corpus: Moonshine
runs on the CPU under a latency budget, so its transcripts are the weakest labels
we will ever have for this audio. Re-running a slower, stronger model offline
over the stored WAVs produces better targets for fine-tuning on one speaker's
voice. The second is a cheap corrector: keeping the *original* Moonshine output
alongside the improved transcript gives aligned (hypothesis, reference) pairs,
which is training data for a small model that fixes Moonshine's habitual
mistakes without paying for a large model at inference time.

Fine-tuning on the stored `text` alone is self-training and will reinforce
existing errors as readily as correct them, which is why the second transcript is
the point rather than a nice-to-have.

Notes:

- **Never overwrite `text`.** It is the corrector's input side. A better
  transcript belongs in an additional field (`text_reference`, or similar) with
  the producing model recorded next to it. Overwriting destroys the pairing the
  corrector needs, irreversibly and silently.
- `post_process_model` on `GET /v1/audio/transcriptions/stream` already does the
  "stronger model on the same audio" step live: it accumulates the session PCM
  and submits it to a second provider as a job
  (`internal/server/server.go`, around the `input_audio.commit` branch).
  **Its result never reaches the corpus** — the refined text is only readable via
  `GET /v1/audio/transcription-jobs/{id}`, so today a caller using it still
  records only the weaker transcript. Wiring that job's result back as a second
  transcript on the same corpus entry would collect the pairs automatically, with
  no offline pass. This is the cheapest path and should be done first.
- The corpus `model` field currently records the provider id (`moonshine`) and
  not the architecture (`MEDIUM_STREAMING`). Error profiles differ per arch, so a
  corpus spanning an arch change would silently mix two distributions with no way
  to separate them. Record the resolved arch before collecting data in volume.
- An offline pass is still needed for audio already captured, and for models too
  slow to sit in a request path. It should read `manifest.jsonl`, skip entries
  that already carry a reference transcript, and append rather than rewrite, so
  it can be interrupted and resumed.
- Prefer a model with a genuinely different error profile for the reference pass,
  not just a bigger Moonshine — agreement between two models with correlated
  failures would overstate label quality. `faster-whisper large-v3` and
  `cohere-transcribe` are already wired and are the obvious first candidates, but
  both are CUDA paths, so this pass wants a GPU that is not busy.
- Human correction still beats every model here for a single-speaker corpus.
  `manifest.jsonl` is line-oriented plain text specifically so entries can be
  hand-edited; a small review tool that plays a clip and edits its text would
  produce better references than any automated pass, and the corrector can be
  trained on those.
