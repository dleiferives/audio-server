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
- Persist optional dialogue/background stems as job artifacts for final TTS
  timing and remixing; the current analysis job consumes the dialogue stem
  internally.
