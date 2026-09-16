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
