# Near-term TODO

- Add providers with native separate speaker and emotion references. Extend
  the multipart speech contract with `emotion_reference` only when the chosen
  provider advertises `reference_modes: ["separate"]`; never silently emulate
  or discard it. IndexTTS2 and Fish Audio S2 are candidates to evaluate.
