# espeak-ng

The `espeak-ng` provider (`internal/provider/espeak`) shells out to the
`espeak-ng` system binary directly — there is no Python sidecar or model to
install here.

Install the binary:

```bash
# Debian/Ubuntu
apt-get install espeak-ng

# macOS
brew install espeak-ng
```

`ffmpeg` is also required for non-WAV output formats (MP3, etc.); the server
pipes `espeak-ng`'s WAV output through it.

This directory exists to keep the per-provider layout under `tts/` consistent
(see [`tts/omnivoice`](../omnivoice/)); it holds setup notes only, no code.
