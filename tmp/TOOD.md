# Near-term TODO

- Add providers with native separate speaker and emotion references. Extend
  the multipart speech contract with `emotion_reference` only when the chosen
  provider advertises `reference_modes: ["separate"]`; never silently emulate
  or discard it. IndexTTS2 and Fish Audio S2 are candidates to evaluate.
- Add true overlapping-actor speech separation (evaluate MossFormer2 or a
  comparable native/open-weight runtime). BS-RoFormer currently separates
  dialogue from background only.
- Add persistent, reviewable cross-asset speaker/character clustering using
  WeSpeaker cosine similarity. Keep the current API's speaker IDs asset-local.
- ~~Persist optional dialogue/background stems as job artifacts~~ — done:
  `POST /v1/audio/separations` (+ `GET .../{id}` and `.../stems/{name}`)
  runs a selectable separator (`bs-roformer` or the new native `htdemucs`,
  audio.cpp GGUF, no Python sidecar) and persists every requested stem via
  the audio store; `analysis-jobs`' `separate_dialogue` path also gained a
  `separation_model` selector.
- ~~DnR-trained cinematic (dialogue/music/effects) separator~~ — done, but
  via a Python sidecar rather than audio.cpp: `separation_model=htdemucs-dnr`
  runs [ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing](https://github.com/ZFTurbo/MVSEP-CDX23-Cinematic-Sound-Demixing)'s
  DnR HTDemucs checkpoint (release `v.1.0.0`, `97d170e1-dbb4db15.th`)
  through the real PyTorch `demucs` package (`separation/htdemucs-dnr/`).
  Verified end-to-end through `/v1/audio/separations`. audio.cpp's native
  HTDemucs C++ port still can't run this checkpoint directly — it
  unconditionally builds a `channel_upsampler`/`channel_downsampler`
  bottleneck projection sized by `config.bottom_channels`
  (`src/models/demucs/pipeline.cpp:1194` and ~1463), with no guard for
  `bottom_channels: 0`, which is this checkpoint's (valid, official) "skip
  the projection, feed full channel width to the transformer" setting —
  crashes with "Tensor dimensions must be positive" instead. Converted
  GGUFs from that dead-end attempt are still sitting at
  `models/HTDemucs-DnR-GGUF/htdemucs-dnr-q8_0.gguf` and `-f16.gguf` if
  someone wants to fix the C++ path later for the native/no-Python route
  (would let this run through the same lifecycle-managed GPU budget as
  the other audiocpp models instead of its own sidecar process).
- Replace the MFA subprocess-per-request forced aligner with a native C++
  implementation. Current cost is ~10.7s per alignment regardless of audio
  length (fixed corpus-DB + worker-pool overhead in `mfa align`, not actual
  compute); MFA's own `align_one` fast path is broken upstream in both 3.4.0
  and 3.4.2 (a Kaldi-level `ContextFst` graph-compiler error for English, and
  a `None` G2P rewriter for Greek) so it isn't a safe workaround.
