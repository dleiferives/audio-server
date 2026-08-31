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
	"github.com/dleiferives/audio-server/internal/provider/align"
	"github.com/dleiferives/audio-server/internal/provider/espeak"
	"github.com/dleiferives/audio-server/internal/provider/fasterwhisper"
	"github.com/dleiferives/audio-server/internal/provider/kokoro"
	"github.com/dleiferives/audio-server/internal/provider/nemotron"
	"github.com/dleiferives/audio-server/internal/provider/omnivoice"
	"github.com/dleiferives/audio-server/internal/provider/parakeet"
	"github.com/dleiferives/audio-server/internal/provider/qwen3asr"
	"github.com/dleiferives/audio-server/internal/provider/supertonic"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/server"
	"github.com/dleiferives/audio-server/internal/store"
	"github.com/dleiferives/audio-server/internal/sttprovider"

	yaml "gopkg.in/yaml.v3"
)

type configFile struct {
	Addr                      string `yaml:"addr"`
	APIKey                    string `yaml:"api_key"`
	WebDir                    string `yaml:"web_dir"`
	AudioStoreDir             string `yaml:"audio_store_dir"`
	AudioTTLSeconds           int    `yaml:"audio_ttl_seconds"`
	MaxInputChars             int    `yaml:"max_input_chars"`
	RequestTimeoutSeconds     int    `yaml:"request_timeout_seconds"`
	MaxConcurrency            int    `yaml:"max_concurrency"`
	EspeakPath                string `yaml:"espeak_path"`
	EspeakDefaultVoice        string `yaml:"espeak_default_voice"`
	MP3Bitrate                string `yaml:"mp3_bitrate"`
	FFmpegPath                string `yaml:"ffmpeg_path"`
	OmnivoiceEnabled          bool   `yaml:"omnivoice_enabled"`
	OmnivoiceAddr             string `yaml:"omnivoice_addr"`
	OmnivoiceConcurrency      int    `yaml:"omnivoice_concurrency"`
	SupertonicAddr            string `yaml:"supertonic_addr"`
	AudiocppBin               string `yaml:"audiocpp_bin"`
	AudiocppIdleUnloadSeconds int    `yaml:"audiocpp_idle_unload_seconds"`
	KokoroEnabled             bool   `yaml:"kokoro_enabled"`
	FasterWhisperEnabled      bool   `yaml:"faster_whisper_enabled"`
	FasterWhisperAddr         string `yaml:"faster_whisper_addr"`
	FasterWhisperPython       string `yaml:"faster_whisper_python"`
	FasterWhisperScript       string `yaml:"faster_whisper_script"`
	FasterWhisperPort         int    `yaml:"faster_whisper_port"`
	FasterWhisperModelSize    string `yaml:"faster_whisper_model_size"`
	FasterWhisperDevice       string `yaml:"faster_whisper_device"`
	FasterWhisperComputeType  string `yaml:"faster_whisper_compute_type"`
	STTEnabled                bool   `yaml:"stt_enabled"`
	DefaultSttProvider        string `yaml:"default_stt_provider"`
	ParakeetEnabled           bool   `yaml:"parakeet_enabled"`
	ParakeetAddr              string `yaml:"parakeet_addr"`
	NemotronAddr              string `yaml:"nemotron_addr"`
	Qwen3ASR06Enabled         bool   `yaml:"qwen3_asr_0_6b_enabled"`
	Qwen3ASR06Addr            string `yaml:"qwen3_asr_0_6b_addr"`
	Qwen3ASR17Enabled         bool   `yaml:"qwen3_asr_1_7b_enabled"`
	Qwen3ASR17Addr            string `yaml:"qwen3_asr_1_7b_addr"`
	AlignEnabled              bool   `yaml:"align_enabled"`
	AlignMFAEnv               string `yaml:"align_mfa_env"`
	AlignMFAWorkDir           string `yaml:"align_mfa_work_dir"`
	AlignMFAModelsConfig      string `yaml:"align_mfa_models_config"`
}

func loadConfig(path string) configFile {
	var cfg configFile
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	yaml.Unmarshal(data, &cfg)
	return cfg
}

func main() {
	cfgPath := flag.String("config", env("AUDIO_CONFIG", "config.yml"), "path to config file")
	flag.Parse()

	yaml.Unmarshal(nil, nil)

	cfg := loadConfig(*cfgPath)

	addr := flag.String("addr", env("AUDIO_ADDR", cfg.Addr), "listen address")
	apiKey := flag.String("api-key", env("AUDIO_API_KEY", cfg.APIKey), "optional bearer API key")
	maxConcurrency := flag.Int("max-concurrency", envInt("AUDIO_MAX_CONCURRENCY", cfg.MaxConcurrency), "maximum concurrent synthesis requests")
	requestTimeoutSeconds := flag.Int("request-timeout-seconds", envInt("AUDIO_REQUEST_TIMEOUT_SECONDS", cfg.RequestTimeoutSeconds), "synthesis request timeout in seconds (0 = disabled)")
	maxInputChars := flag.Int("max-input-chars", envInt("AUDIO_MAX_INPUT_CHARS", cfg.MaxInputChars), "maximum input length in characters")
	espeakPath := flag.String("espeak-path", env("AUDIO_ESPEAK_PATH", cfg.EspeakPath), "espeak-ng binary path")
	ffmpegPath := flag.String("ffmpeg-path", env("AUDIO_FFMPEG_PATH", cfg.FFmpegPath), "ffmpeg binary path")
	mp3Bitrate := flag.String("mp3-bitrate", env("AUDIO_MP3_BITRATE", cfg.MP3Bitrate), "mp3 bitrate for generated speech")
	defaultProvider := flag.String("default-provider", env("AUDIO_DEFAULT_PROVIDER", "espeak-ng"), "default provider id")
	defaultVoice := flag.String("espeak-default-voice", env("AUDIO_ESPEAK_DEFAULT_VOICE", cfg.EspeakDefaultVoice), "default eSpeak voice")
	omnivoiceAddr := flag.String("omnivoice-addr", env("AUDIO_OMNIVOICE_ADDR", cfg.OmnivoiceAddr), "OmniVoice sidecar base URL (e.g. http://127.0.0.1:8020); disabled when blank")
	omnivoiceConcurrency := flag.Int("omnivoice-concurrency", envInt("AUDIO_OMNIVOICE_CONCURRENCY", cfg.OmnivoiceConcurrency), "concurrent OmniVoice workers")
	supertonicAddr := flag.String("supertonic-addr", env("AUDIO_SUPERTONIC_ADDR", cfg.SupertonicAddr), "Supertonic sidecar base URL (e.g. http://127.0.0.1:8022); disabled when blank")
	audiocppBin := flag.String("audiocpp-bin", env("AUDIO_AUDIOCPP_BIN", cfg.AudiocppBin), "path to audiocpp_server binary")
	audiocppIdle := flag.Int("audiocpp-idle-unload-seconds", envInt("AUDIO_AUDIOCPP_IDLE_UNLOAD", cfg.AudiocppIdleUnloadSeconds), "seconds before unloading idle audiocpp_server instances")
	kokoroEnabled := flag.Bool("kokoro-enabled", cfg.KokoroEnabled, "enable Kokoro TTS provider")
	fasterWhisperEnabled := flag.Bool("faster-whisper-enabled", cfg.FasterWhisperEnabled, "enable faster-whisper STT provider")
	fasterWhisperAddr := flag.String("faster-whisper-addr", env("AUDIO_FASTERWHISPER_ADDR", valueOr(cfg.FasterWhisperAddr, "http://127.0.0.1:8030")), "faster-whisper sidecar base URL")
	fasterWhisperPython := flag.String("faster-whisper-python", env("AUDIO_FASTERWHISPER_PYTHON", valueOr(cfg.FasterWhisperPython, "python3")), "Python executable for the faster-whisper sidecar")
	fasterWhisperScript := flag.String("faster-whisper-script", env("AUDIO_FASTERWHISPER_SCRIPT", valueOr(cfg.FasterWhisperScript, "stt/fasterwhisper/server.py")), "path to the faster-whisper sidecar script")
	fasterWhisperPort := flag.Int("faster-whisper-port", envInt("AUDIO_FASTERWHISPER_PORT", intOr(cfg.FasterWhisperPort, 8030)), "local faster-whisper sidecar port")
	fasterWhisperModelSize := flag.String("faster-whisper-model-size", env("AUDIO_FASTERWHISPER_MODEL_SIZE", valueOr(cfg.FasterWhisperModelSize, "small")), "faster-whisper model size")
	fasterWhisperDevice := flag.String("faster-whisper-device", env("AUDIO_FASTERWHISPER_DEVICE", valueOr(cfg.FasterWhisperDevice, "auto")), "faster-whisper inference device")
	fasterWhisperComputeType := flag.String("faster-whisper-compute-type", env("AUDIO_FASTERWHISPER_COMPUTE_TYPE", valueOr(cfg.FasterWhisperComputeType, "default")), "faster-whisper compute type")
	sttEnabled := flag.Bool("stt-enabled", cfg.STTEnabled, "enable audio.cpp STT providers")
	defaultSttProvider := flag.String("default-stt-provider", env("AUDIO_DEFAULT_STT_PROVIDER", cfg.DefaultSttProvider), "provider used for auto, whisper-1, and blank transcription models")
	parakeetEnabled := flag.Bool("parakeet-enabled", cfg.ParakeetEnabled, "enable Parakeet-TDT ASR provider")
	parakeetAddr := flag.String("parakeet-addr", env("AUDIO_PARAKEET_ADDR", cfg.ParakeetAddr), "Parakeet-TDT ASR sidecar base URL")
	nemotronAddr := flag.String("nemotron-addr", env("AUDIO_NEMOTRON_ADDR", cfg.NemotronAddr), "Nemotron ASR sidecar base URL")
	qwen3ASR06Enabled := flag.Bool("qwen3-asr-0.6b-enabled", cfg.Qwen3ASR06Enabled, "enable Qwen3-ASR 0.6B provider")
	qwen3ASR06Addr := flag.String("qwen3-asr-0.6b-addr", env("AUDIO_QWEN3_ASR_0_6B_ADDR", valueOr(cfg.Qwen3ASR06Addr, "http://127.0.0.1:8027")), "Qwen3-ASR 0.6B sidecar base URL")
	qwen3ASR17Enabled := flag.Bool("qwen3-asr-1.7b-enabled", cfg.Qwen3ASR17Enabled, "enable Qwen3-ASR 1.7B provider")
	qwen3ASR17Addr := flag.String("qwen3-asr-1.7b-addr", env("AUDIO_QWEN3_ASR_1_7B_ADDR", valueOr(cfg.Qwen3ASR17Addr, "http://127.0.0.1:8028")), "Qwen3-ASR 1.7B sidecar base URL")
	webDir := flag.String("web-dir", env("AUDIO_WEB_DIR", cfg.WebDir), "optional path to static web frontend directory")
	audioTTL := flag.Int("audio-ttl-seconds", envInt("AUDIO_AUDIO_TTL_SECONDS", cfg.AudioTTLSeconds), "audio file retention in seconds (0 = forever)")
	audioStoreDir := flag.String("audio-store-dir", env("AUDIO_STORE_DIR", cfg.AudioStoreDir), "directory for generated audio files (empty = in-memory)")
	alignEnabled := flag.Bool("align-enabled", cfg.AlignEnabled, "enable MFA forced alignment")
	alignMFAEnv := flag.String("align-mfa-env", cfg.AlignMFAEnv, "path to MFA micromamba environment")
	alignMFAWorkDir := flag.String("align-mfa-work-dir", cfg.AlignMFAWorkDir, "MFA working directory")
	alignMFAModelsConfig := flag.String("align-mfa-models-config", cfg.AlignMFAModelsConfig, "path to language model YAML config")
	flag.Parse()

	encoder := encode.NewFFmpeg(*ffmpegPath, *mp3Bitrate)
	espeakProvider := espeak.New(*espeakPath, *defaultVoice, encoder)
	providers := []provider.Provider{espeakProvider}
	workers := map[string]int{espeakProvider.ID(): *maxConcurrency}

	var gpuLifecycle *lifecycle.Manager
	if strings.TrimSpace(*audiocppBin) != "" || *fasterWhisperEnabled {
		gpuLifecycle = lifecycle.NewManager(*audiocppBin)
		idleDelay := time.Duration(*audiocppIdle) * time.Second
		if strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*omnivoiceAddr) != "" {
			gpuLifecycle.Register("omnivoice", "audiocpp-configs/omnivoice.json", 8020, idleDelay)
		}
		if strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*supertonicAddr) != "" {
			gpuLifecycle.Register("supertonic", "audiocpp-configs/supertonic.json", 8022, idleDelay)
		}
		if strings.TrimSpace(*audiocppBin) != "" && *sttEnabled && *parakeetEnabled && strings.TrimSpace(*parakeetAddr) != "" {
			gpuLifecycle.Register("parakeet", "audiocpp-configs/parakeet.json", 8026, idleDelay)
		}
		if strings.TrimSpace(*audiocppBin) != "" && *sttEnabled && strings.TrimSpace(*nemotronAddr) != "" {
			gpuLifecycle.Register("nemotron", "audiocpp-configs/nemotron.json", 8024, idleDelay)
		}
		if strings.TrimSpace(*audiocppBin) != "" && *qwen3ASR06Enabled {
			gpuLifecycle.Register("qwen3-asr-0.6b", "audiocpp-configs/qwen3-asr-0.6b.json", 8027, idleDelay)
		}
		if strings.TrimSpace(*audiocppBin) != "" && *qwen3ASR17Enabled {
			gpuLifecycle.Register("qwen3-asr-1.7b", "audiocpp-configs/qwen3-asr-1.7b.json", 8028, idleDelay)
		}
		if *fasterWhisperEnabled && strings.TrimSpace(*fasterWhisperPython) != "" && strings.TrimSpace(*fasterWhisperScript) != "" {
			gpuLifecycle.RegisterCommand(
				"faster-whisper",
				*fasterWhisperPython,
				[]string{
					*fasterWhisperScript,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*fasterWhisperPort),
					"--model-size", *fasterWhisperModelSize,
					"--device", *fasterWhisperDevice,
					"--compute-type", *fasterWhisperComputeType,
					"--idle-unload-seconds", "0",
				},
				strings.TrimRight(*fasterWhisperAddr, "/")+"/health",
				idleDelay,
			)
		}
	}

	if strings.TrimSpace(*omnivoiceAddr) != "" {
		ov := omnivoice.New(*omnivoiceAddr, nil, encoder)
		if gpuLifecycle != nil {
			ov.StartFunc = func() error { return gpuLifecycle.Start("omnivoice", true) }
		}
		providers = append(providers, ov)
		workers["omnivoice"] = *omnivoiceConcurrency
	}
	if strings.TrimSpace(*supertonicAddr) != "" {
		st := supertonic.New(*supertonicAddr, nil, encoder)
		if gpuLifecycle != nil {
			st.StartFunc = func() error { return gpuLifecycle.Start("supertonic", true) }
		}
		providers = append(providers, st)
		workers["supertonic"] = 2
	}
	if *kokoroEnabled {
		kokoroAddr := env("AUDIO_KOKORO_ADDR", "http://127.0.0.1:8021")
		providers = append(providers, kokoro.New(kokoroAddr, nil, encoder))
		workers["kokoro"] = 1
	}

	providerMap := make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		providerMap[p.ID()] = p
	}
	resourceGroups := map[string]string{}
	resourceSwitchDelay := map[string]time.Duration{}
	idleUnload := map[string]time.Duration{}
	if gpuLifecycle != nil {
		const gpuGroup = "audiocpp-gpu"
		resourceGroups["omnivoice"] = gpuGroup
		resourceGroups["supertonic"] = gpuGroup
		idleDelay := time.Duration(*audiocppIdle) * time.Second
		idleUnload["omnivoice"] = idleDelay
		idleUnload["supertonic"] = idleDelay
	}

	requestTimeout := time.Duration(*requestTimeoutSeconds) * time.Second
	jobQueue := queue.NewManager(queue.Config{
		Providers:           providerMap,
		Workers:             workers,
		IdleUnload:          idleUnload,
		ResourceGroups:      resourceGroups,
		ResourceSwitchDelay: resourceSwitchDelay,
		SynthesizeTimeout:   requestTimeout,
	})

	var sttProviders []sttprovider.Provider
	if *sttEnabled && *parakeetEnabled && strings.TrimSpace(*parakeetAddr) != "" {
		pk := parakeet.New(*parakeetAddr, nil)
		if gpuLifecycle != nil {
			pk.StartFunc = func() error { return gpuLifecycle.Start("parakeet", true) }
		}
		sttProviders = append(sttProviders, pk)
	}
	if *fasterWhisperEnabled {
		fw := fasterwhisper.New(*fasterWhisperAddr, nil)
		if gpuLifecycle != nil && strings.TrimSpace(*fasterWhisperPython) != "" && strings.TrimSpace(*fasterWhisperScript) != "" {
			fw.StartFunc = func() error { return gpuLifecycle.Start("faster-whisper", true) }
		}
		sttProviders = append(sttProviders, fw)
	}
	if *sttEnabled && strings.TrimSpace(*nemotronAddr) != "" {
		nm := nemotron.New(*nemotronAddr, nil)
		if gpuLifecycle != nil {
			nm.StartFunc = func() error { return gpuLifecycle.Start("nemotron", true) }
		}
		sttProviders = append(sttProviders, nm)
	}
	if *qwen3ASR06Enabled {
		qw := qwen3asr.New("qwen3-asr-0.6b", "qwen3-asr-0.6b", *qwen3ASR06Addr, nil)
		if gpuLifecycle != nil {
			qw.StartFunc = func() error { return gpuLifecycle.Start("qwen3-asr-0.6b", true) }
		}
		sttProviders = append(sttProviders, qw)
	}
	if *qwen3ASR17Enabled {
		qw := qwen3asr.New("qwen3-asr-1.7b", "qwen3-asr-1.7b", *qwen3ASR17Addr, nil)
		if gpuLifecycle != nil {
			qw.StartFunc = func() error { return gpuLifecycle.Start("qwen3-asr-1.7b", true) }
		}
		sttProviders = append(sttProviders, qw)
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

	var alignProvider server.Aligner
	if *alignEnabled {
		langMap := loadAlignLanguages(*alignMFAModelsConfig)
		alignProvider = align.NewAlignProvider(*alignMFAEnv, *alignMFAWorkDir, "", langMap)
		log.Printf("alignment: enabled (%d languages)", len(langMap))
	}

	audioServer, err := server.New(server.Config{
		Providers:          providers,
		DefaultProvider:    *defaultProvider,
		APIKey:             *apiKey,
		MaxInputChars:      *maxInputChars,
		RequestTimeout:     requestTimeout,
		Queue:              jobQueue,
		StreamWorkers:      workers,
		SttProviders:       sttProviders,
		DefaultSttProvider: *defaultSttProvider,
		SttAudioNormalizer: encoder,
		WebDir:             *webDir,
		AudioStore:         audioStore,
		AlignProvider:      alignProvider,
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

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func intOr(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func loadAlignLanguages(path string) map[string]align.LanguageModel {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("alignment: could not read language config: %v", err)
		return nil
	}
	var cfg struct {
		Models map[string]align.LanguageModel `yaml:"models"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Printf("alignment: invalid language config: %v", err)
		return nil
	}
	return cfg.Models
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
