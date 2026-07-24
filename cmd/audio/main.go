// Command audio runs the TIFL audio manager service.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dleiferives/audio-server/internal/encode"
	"github.com/dleiferives/audio-server/internal/lifecycle"
	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/provider/espeak"
	"github.com/dleiferives/audio-server/internal/provider/fasterwhisper"
	"github.com/dleiferives/audio-server/internal/provider/kokoro"
	"github.com/dleiferives/audio-server/internal/provider/omnivoice"
	"github.com/dleiferives/audio-server/internal/provider/supertonic"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/server"
	"github.com/dleiferives/audio-server/internal/store"
	"github.com/dleiferives/audio-server/internal/sttprovider"
)

func main() {
	addr := flag.String("addr", env("AUDIO_ADDR", "127.0.0.1:8010"), "listen address")
	apiKey := flag.String("api-key", env("AUDIO_API_KEY", ""), "optional bearer API key")
	maxConcurrency := flag.Int("max-concurrency", envInt("AUDIO_MAX_CONCURRENCY", 2), "maximum concurrent synthesis requests")
	requestTimeoutSeconds := flag.Int("request-timeout-seconds", envInt("AUDIO_REQUEST_TIMEOUT_SECONDS", 0), "synthesis request timeout in seconds (0 = disabled)")
	maxInputChars := flag.Int("max-input-chars", envInt("AUDIO_MAX_INPUT_CHARS", 1000000), "maximum input length in characters")
	espeakPath := flag.String("espeak-path", env("AUDIO_ESPEAK_PATH", "espeak-ng"), "espeak-ng binary path")
	ffmpegPath := flag.String("ffmpeg-path", env("AUDIO_FFMPEG_PATH", "ffmpeg"), "ffmpeg binary path")
	mp3Bitrate := flag.String("mp3-bitrate", env("AUDIO_MP3_BITRATE", "48k"), "mp3 bitrate for generated speech")
	defaultProvider := flag.String("default-provider", env("AUDIO_DEFAULT_PROVIDER", "espeak-ng"), "default provider id")
	defaultVoice := flag.String("espeak-default-voice", env("AUDIO_ESPEAK_DEFAULT_VOICE", "en"), "default eSpeak voice")
	omnivoiceAddr := flag.String("omnivoice-addr", env("AUDIO_OMNIVOICE_ADDR", ""), "OmniVoice sidecar base URL (e.g. http://127.0.0.1:8020); disabled when blank")
	omnivoiceConcurrency := flag.Int("omnivoice-concurrency", envInt("AUDIO_OMNIVOICE_CONCURRENCY", 1), "concurrent OmniVoice workers")
	supertonicAddr := flag.String("supertonic-addr", env("AUDIO_SUPERTONIC_ADDR", ""), "Supertonic sidecar base URL (e.g. http://127.0.0.1:8022); disabled when blank")
	audiocppBin := flag.String("audiocpp-bin", env("AUDIO_AUDIOCPP_BIN", "audio.cpp/build/linux-cuda-release/bin/audiocpp_server"), "path to audiocpp_server binary")
	audiocppIdle := flag.Int("audiocpp-idle-unload-seconds", envInt("AUDIO_AUDIOCPP_IDLE_UNLOAD", 600), "seconds before unloading idle audiocpp_server instances")
	kokoroAddr := flag.String("kokoro-addr", env("AUDIO_KOKORO_ADDR", ""), "Kokoro TTS sidecar base URL (e.g. http://127.0.0.1:8021); disabled when blank")
	kokoroConcurrency := flag.Int("kokoro-concurrency", envInt("AUDIO_KOKORO_CONCURRENCY", 1), "concurrent Kokoro workers")
	_ = flag.Int("kokoro-idle-unload-seconds", envInt("AUDIO_KOKORO_IDLE_UNLOAD_SECONDS", 30), "seconds an empty Kokoro queue waits before the model is unloaded")
	_ = flag.Int("model-idle-unload-seconds", envInt("AUDIO_MODEL_IDLE_UNLOAD_SECONDS", 600), "seconds before unloading idle GPU models (default 10 min)")
	_ = flag.String("audiocpp-bin", env("AUDIO_AUDIOCPP_BIN", "audio.cpp/build/linux-cuda-release/bin/audiocpp_server"), "path to audiocpp_server binary")
	_ = flag.String("audiocpp-omnivoice-cfg", env("AUDIO_AUDIOCPP_OMNIVOICE_CFG", "audio.cpp/omnivoice-config.json"), "config for OmniVoice instance")
	_ = flag.String("audiocpp-supertonic-cfg", env("AUDIO_AUDIOCPP_SUPERTONIC_CFG", "audio.cpp/supertonic-config.json"), "config for Supertonic instance")
	fasterWhisperAddr := flag.String("faster-whisper-addr", env("AUDIO_FASTERWHISPER_ADDR", ""), "faster-whisper sidecar base URL (e.g. http://127.0.0.1:8030); disabled when blank")
	webDir := flag.String("web-dir", env("AUDIO_WEB_DIR", ""), "optional path to static web frontend directory")
	audioTTL := flag.Int("audio-ttl-seconds", envInt("AUDIO_AUDIO_TTL_SECONDS", 0), "audio file retention in seconds (0 = forever)")
	audioStoreDir := flag.String("audio-store-dir", env("AUDIO_STORE_DIR", ""), "directory for generated audio files (empty = in-memory)")
	_ = flag.Int("supertonic-port", envInt("AUDIO_SUPERTONIC_PORT", 8022), "DEPRECATED — shares same audiocpp_server as omnivoice")
	flag.Parse()

	encoder := encode.NewFFmpeg(*ffmpegPath, *mp3Bitrate)
	espeakProvider := espeak.New(*espeakPath, *defaultVoice, encoder)
	providers := []provider.Provider{espeakProvider}
	workers := map[string]int{espeakProvider.ID(): *maxConcurrency}

	var gpuLifecycle *lifecycle.Manager
	if strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*omnivoiceAddr) != "" {
		gpuLifecycle = lifecycle.NewManager(*audiocppBin)
		gpuLifecycle.Register("omnivoice", "audio.cpp/omnivoice-config.json", 8020, time.Duration(*audiocppIdle)*time.Second)
		gpuLifecycle.Register("supertonic", "audio.cpp/supertonic-config.json", 8022, time.Duration(*audiocppIdle)*time.Second)
	}

	if strings.TrimSpace(*omnivoiceAddr) != "" {
		providers = append(providers, omnivoice.New(*omnivoiceAddr, nil, encoder))
		workers["omnivoice"] = *omnivoiceConcurrency
	}
	if strings.TrimSpace(*supertonicAddr) != "" {
		providers = append(providers, supertonic.New(*supertonicAddr, nil, encoder))
		workers["supertonic"] = 2
	}
	if strings.TrimSpace(*kokoroAddr) != "" {
		providers = append(providers, kokoro.New(*kokoroAddr, nil, encoder))
		workers["kokoro"] = *kokoroConcurrency
	}

	providerMap := make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		providerMap[p.ID()] = p
	}

	requestTimeout := time.Duration(*requestTimeoutSeconds) * time.Second
	jobQueue := queue.NewManager(queue.Config{
		Providers:         providerMap,
		Workers:           workers,
		SynthesizeTimeout: requestTimeout,
	})

	var sttProviders []sttprovider.Provider
	if strings.TrimSpace(*fasterWhisperAddr) != "" {
		sttProviders = append(sttProviders, fasterwhisper.New(*fasterWhisperAddr, nil))
	}

	var audioStore *store.Store
	if strings.TrimSpace(*audioStoreDir) != "" {
		s, err := store.New(*audioStoreDir, time.Duration(*audioTTL)*time.Second)
		if err != nil {
			log.Fatalf("audio store: %v", err)
		}
		audioStore = s
		log.Printf("audio store: %s (ttl=%s)", *audioStoreDir, time.Duration(*audioTTL)*time.Second)
	}

	audioServer, err := server.New(server.Config{
		Providers:       providers,
		DefaultProvider: *defaultProvider,
		APIKey:          *apiKey,
		MaxInputChars:   *maxInputChars,
		RequestTimeout:  requestTimeout,
		Queue:           jobQueue,
		StreamWorkers:   workers,
		SttProviders:    sttProviders,
		WebDir:          *webDir,
		AudioStore:      audioStore,
	})
	if err != nil {
		log.Fatalf("audio server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           audioServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("serving on %s", httpURL(*addr))
	errc := make(chan error, 1)
	go func() { errc <- httpServer.ListenAndServe() }()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case sig := <-sigc:
		log.Printf("shutting down after %s", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}
	if gpuLifecycle != nil {
		gpuLifecycle.StopAll()
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func httpURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
