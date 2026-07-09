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
	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/provider/espeak"
	"github.com/dleiferives/audio-server/internal/provider/kokoro"
	"github.com/dleiferives/audio-server/internal/provider/omnivoice"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/server"
)

func main() {
	addr := flag.String("addr", env("AUDIO_ADDR", "127.0.0.1:8010"), "listen address")
	apiKey := flag.String("api-key", env("AUDIO_API_KEY", ""), "optional bearer API key")
	maxConcurrency := flag.Int("max-concurrency", envInt("AUDIO_MAX_CONCURRENCY", 2), "maximum concurrent synthesis requests")
	requestTimeoutSeconds := flag.Int("request-timeout-seconds", envInt("AUDIO_REQUEST_TIMEOUT_SECONDS", 30), "synthesis request timeout in seconds")
	maxInputChars := flag.Int("max-input-chars", envInt("AUDIO_MAX_INPUT_CHARS", 5000), "maximum input length in characters")
	espeakPath := flag.String("espeak-path", env("AUDIO_ESPEAK_PATH", "espeak-ng"), "espeak-ng binary path")
	ffmpegPath := flag.String("ffmpeg-path", env("AUDIO_FFMPEG_PATH", "ffmpeg"), "ffmpeg binary path")
	mp3Bitrate := flag.String("mp3-bitrate", env("AUDIO_MP3_BITRATE", "48k"), "mp3 bitrate for generated speech")
	defaultProvider := flag.String("default-provider", env("AUDIO_DEFAULT_PROVIDER", "espeak-ng"), "default provider id")
	defaultVoice := flag.String("espeak-default-voice", env("AUDIO_ESPEAK_DEFAULT_VOICE", "en"), "default eSpeak voice")
	omnivoiceAddr := flag.String("omnivoice-addr", env("AUDIO_OMNIVOICE_ADDR", ""), "OmniVoice sidecar base URL (e.g. http://127.0.0.1:8020); disabled when blank")
	omnivoiceConcurrency := flag.Int("omnivoice-concurrency", envInt("AUDIO_OMNIVOICE_CONCURRENCY", 1), "concurrent OmniVoice workers (keep at 1 on limited VRAM)")
	omnivoiceIdleUnloadSeconds := flag.Int("omnivoice-idle-unload-seconds", envInt("AUDIO_OMNIVOICE_IDLE_UNLOAD_SECONDS", 30), "seconds an empty OmniVoice queue waits before the model is unloaded")
	kokoroAddr := flag.String("kokoro-addr", env("AUDIO_KOKORO_ADDR", ""), "Kokoro TTS sidecar base URL (e.g. http://127.0.0.1:8021); disabled when blank")
	kokoroConcurrency := flag.Int("kokoro-concurrency", envInt("AUDIO_KOKORO_CONCURRENCY", 1), "concurrent Kokoro workers (keep at 1 on limited VRAM)")
	kokoroIdleUnloadSeconds := flag.Int("kokoro-idle-unload-seconds", envInt("AUDIO_KOKORO_IDLE_UNLOAD_SECONDS", 30), "seconds an empty Kokoro queue waits before the model is unloaded")
	flag.Parse()

	encoder := encode.NewFFmpeg(*ffmpegPath, *mp3Bitrate)
	espeakProvider := espeak.New(*espeakPath, *defaultVoice, encoder)
	providers := []provider.Provider{espeakProvider}
	workers := map[string]int{espeakProvider.ID(): *maxConcurrency}
	idleUnload := map[string]time.Duration{}
	if strings.TrimSpace(*omnivoiceAddr) != "" {
		omnivoiceProvider := omnivoice.New(*omnivoiceAddr, nil, encoder)
		providers = append(providers, omnivoiceProvider)
		workers[omnivoiceProvider.ID()] = *omnivoiceConcurrency
		idleUnload[omnivoiceProvider.ID()] = time.Duration(*omnivoiceIdleUnloadSeconds) * time.Second
	}
	if strings.TrimSpace(*kokoroAddr) != "" {
		kokoroProvider := kokoro.New(*kokoroAddr, nil, encoder)
		providers = append(providers, kokoroProvider)
		workers[kokoroProvider.ID()] = *kokoroConcurrency
		idleUnload[kokoroProvider.ID()] = time.Duration(*kokoroIdleUnloadSeconds) * time.Second
	}

	providerMap := make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		providerMap[p.ID()] = p
	}
	requestTimeout := time.Duration(*requestTimeoutSeconds) * time.Second
	jobQueue := queue.NewManager(queue.Config{
		Providers:         providerMap,
		Workers:           workers,
		IdleUnload:        idleUnload,
		SynthesizeTimeout: requestTimeout,
	})

	audioServer, err := server.New(server.Config{
		Providers:       providers,
		DefaultProvider: *defaultProvider,
		APIKey:          *apiKey,
		MaxInputChars:   *maxInputChars,
		RequestTimeout:  requestTimeout,
		Queue:           jobQueue,
		StreamWorkers:   workers,
	})
	if err != nil {
		log.Fatalf("audio server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           audioServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("audio server listening on %s", httpURL(*addr))
		errc <- httpServer.ListenAndServe()
	}()

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
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Fatalf("shutdown: %v", err)
		}
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
