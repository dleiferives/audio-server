// Command audio runs the TIFL audio manager service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	"github.com/dleiferives/audio-server/internal/provider/transcribecpp"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/server"
	"github.com/dleiferives/audio-server/internal/store"
	"github.com/dleiferives/audio-server/internal/sttprovider"

	yaml "gopkg.in/yaml.v3"
)

type configFile struct {
	Addr                      string         `yaml:"addr"`
	APIKey                    string         `yaml:"api_key"`
	WebDir                    string         `yaml:"web_dir"`
	AudioStoreDir             string         `yaml:"audio_store_dir"`
	AudioTTLSeconds           int            `yaml:"audio_ttl_seconds"`
	MaxInputChars             int            `yaml:"max_input_chars"`
	RequestTimeoutSeconds     int            `yaml:"request_timeout_seconds"`
	MaxConcurrency            int            `yaml:"max_concurrency"`
	EspeakPath                string         `yaml:"espeak_path"`
	EspeakDefaultVoice        string         `yaml:"espeak_default_voice"`
	MP3Bitrate                string         `yaml:"mp3_bitrate"`
	FFmpegPath                string         `yaml:"ffmpeg_path"`
	OmnivoiceEnabled          bool           `yaml:"omnivoice_enabled"`
	OmnivoiceAddr             string         `yaml:"omnivoice_addr"`
	OmnivoiceConcurrency      int            `yaml:"omnivoice_concurrency"`
	SupertonicAddr            string         `yaml:"supertonic_addr"`
	AudiocppBin               string         `yaml:"audiocpp_bin"`
	AudiocppIdleUnloadSeconds int            `yaml:"audiocpp_idle_unload_seconds"`
	MaxVRAMMiB                any            `yaml:"max_vram_mib"`
	ModelVRAMMiB              map[string]int `yaml:"model_vram_mib"`
	KokoroEnabled             bool           `yaml:"kokoro_enabled"`
	FasterWhisperEnabled      bool           `yaml:"faster_whisper_enabled"`
	FasterWhisperAddr         string         `yaml:"faster_whisper_addr"`
	FasterWhisperPython       string         `yaml:"faster_whisper_python"`
	FasterWhisperScript       string         `yaml:"faster_whisper_script"`
	FasterWhisperPort         int            `yaml:"faster_whisper_port"`
	FasterWhisperModelSize    string         `yaml:"faster_whisper_model_size"`
	FasterWhisperDevice       string         `yaml:"faster_whisper_device"`
	FasterWhisperComputeType  string         `yaml:"faster_whisper_compute_type"`
	STTEnabled                bool           `yaml:"stt_enabled"`
	DefaultSttProvider        string         `yaml:"default_stt_provider"`
	ParakeetEnabled           bool           `yaml:"parakeet_enabled"`
	ParakeetAddr              string         `yaml:"parakeet_addr"`
	NemotronAddr              string         `yaml:"nemotron_addr"`
	Qwen3ASR06Enabled         bool           `yaml:"qwen3_asr_0_6b_enabled"`
	Qwen3ASR06Addr            string         `yaml:"qwen3_asr_0_6b_addr"`
	Qwen3ASR17Enabled         bool           `yaml:"qwen3_asr_1_7b_enabled"`
	Qwen3ASR17Addr            string         `yaml:"qwen3_asr_1_7b_addr"`
	TranscribecppEnabled      bool           `yaml:"transcribecpp_enabled"`
	TranscribecppProviderID   string         `yaml:"transcribecpp_provider_id"`
	TranscribecppAddr         string         `yaml:"transcribecpp_addr"`
	TranscribecppBin          string         `yaml:"transcribecpp_bin"`
	TranscribecppPort         int            `yaml:"transcribecpp_port"`
	TranscribecppModel        string         `yaml:"transcribecpp_model"`
	TranscribecppBackend      string         `yaml:"transcribecpp_backend"`
	TranscribecppThreads      int            `yaml:"transcribecpp_threads"`
	VoxtralRealtimeEnabled    bool           `yaml:"voxtral_realtime_enabled"`
	VoxtralRealtimeAddr       string         `yaml:"voxtral_realtime_addr"`
	VoxtralRealtimePort       int            `yaml:"voxtral_realtime_port"`
	VoxtralRealtimeModel      string         `yaml:"voxtral_realtime_model"`
	VoxtralRealtimeBackend    string         `yaml:"voxtral_realtime_backend"`
	VoxtralRealtimeThreads    int            `yaml:"voxtral_realtime_threads"`
	AlignEnabled              bool           `yaml:"align_enabled"`
	AlignMFAEnv               string         `yaml:"align_mfa_env"`
	AlignMFAWorkDir           string         `yaml:"align_mfa_work_dir"`
	AlignMFAModelsConfig      string         `yaml:"align_mfa_models_config"`
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
	configPath := configPathFromArgs(os.Args[1:], env("AUDIO_CONFIG", "config.yml"))
	cfg := loadConfig(configPath)

	flag.String("config", configPath, "path to config file")
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
	maxVRAM := flag.String("max-vram-mib", env("AUDIO_MAX_VRAM_MIB", vramLimitString(cfg.MaxVRAMMiB)), "GPU residency budget in MiB, or auto")
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
	transcribecppEnabled := flag.Bool("transcribecpp-enabled", cfg.TranscribecppEnabled, "enable the native transcribe.cpp STT provider")
	transcribecppProviderID := flag.String("transcribecpp-provider-id", env("AUDIO_TRANSCRIBECPP_PROVIDER_ID", valueOr(cfg.TranscribecppProviderID, "cohere-transcribe")), "provider/model id exposed by the transcribe.cpp sidecar")
	transcribecppAddr := flag.String("transcribecpp-addr", env("AUDIO_TRANSCRIBECPP_ADDR", valueOr(cfg.TranscribecppAddr, "http://127.0.0.1:8031")), "transcribe.cpp sidecar base URL")
	transcribecppBin := flag.String("transcribecpp-bin", env("AUDIO_TRANSCRIBECPP_BIN", valueOr(cfg.TranscribecppBin, "bin/transcribecpp_server")), "path to the transcribe.cpp sidecar binary")
	transcribecppPort := flag.Int("transcribecpp-port", envInt("AUDIO_TRANSCRIBECPP_PORT", intOr(cfg.TranscribecppPort, 8031)), "local transcribe.cpp sidecar port")
	transcribecppModel := flag.String("transcribecpp-model", env("AUDIO_TRANSCRIBECPP_MODEL", cfg.TranscribecppModel), "path to any transcribe.cpp-compatible GGUF model")
	transcribecppBackend := flag.String("transcribecpp-backend", env("AUDIO_TRANSCRIBECPP_BACKEND", valueOr(cfg.TranscribecppBackend, "cuda")), "transcribe.cpp backend: auto, cuda, or cpu")
	transcribecppThreads := flag.Int("transcribecpp-threads", envInt("AUDIO_TRANSCRIBECPP_THREADS", cfg.TranscribecppThreads), "transcribe.cpp CPU thread count (0 = automatic)")
	voxtralRealtimeEnabled := flag.Bool("voxtral-realtime-enabled", cfg.VoxtralRealtimeEnabled, "enable native Voxtral Realtime live STT")
	voxtralRealtimeAddr := flag.String("voxtral-realtime-addr", env("AUDIO_VOXTRAL_REALTIME_ADDR", valueOr(cfg.VoxtralRealtimeAddr, "http://127.0.0.1:8032")), "Voxtral Realtime sidecar base URL")
	voxtralRealtimePort := flag.Int("voxtral-realtime-port", envInt("AUDIO_VOXTRAL_REALTIME_PORT", intOr(cfg.VoxtralRealtimePort, 8032)), "local Voxtral Realtime sidecar port")
	voxtralRealtimeModel := flag.String("voxtral-realtime-model", env("AUDIO_VOXTRAL_REALTIME_MODEL", cfg.VoxtralRealtimeModel), "path to the Voxtral Realtime GGUF model")
	voxtralRealtimeBackend := flag.String("voxtral-realtime-backend", env("AUDIO_VOXTRAL_REALTIME_BACKEND", valueOr(cfg.VoxtralRealtimeBackend, "cuda")), "Voxtral Realtime backend: auto, cuda, or cpu")
	voxtralRealtimeThreads := flag.Int("voxtral-realtime-threads", envInt("AUDIO_VOXTRAL_REALTIME_THREADS", cfg.VoxtralRealtimeThreads), "Voxtral Realtime CPU thread count (0 = automatic)")
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
	if strings.TrimSpace(*audiocppBin) != "" || *fasterWhisperEnabled || *transcribecppEnabled || *voxtralRealtimeEnabled {
		maxVRAMMiB, err := resolveVRAMLimit(*maxVRAM)
		if err != nil {
			log.Fatalf("GPU VRAM configuration: %v", err)
		}
		gpuLifecycle = lifecycle.NewManager(*audiocppBin, lifecycle.Config{MaxVRAMMiB: maxVRAMMiB})
		if maxVRAMMiB > 0 {
			log.Printf("GPU residency budget: %d MiB", maxVRAMMiB)
		} else {
			log.Printf("GPU residency budget: unavailable; budget eviction disabled")
		}
		idleDelay := time.Duration(*audiocppIdle) * time.Second
		if strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*omnivoiceAddr) != "" {
			gpuLifecycle.RegisterModel("omnivoice", "audiocpp-configs/omnivoice.json", 8020, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "omnivoice", 2048))
		}
		if strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*supertonicAddr) != "" {
			gpuLifecycle.RegisterModel("supertonic", "audiocpp-configs/supertonic.json", 8022, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "supertonic", 1024))
		}
		if strings.TrimSpace(*audiocppBin) != "" && *sttEnabled && *parakeetEnabled && strings.TrimSpace(*parakeetAddr) != "" {
			gpuLifecycle.RegisterModel("parakeet", "audiocpp-configs/parakeet.json", 8026, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "parakeet", 1280))
		}
		if strings.TrimSpace(*audiocppBin) != "" && *sttEnabled && strings.TrimSpace(*nemotronAddr) != "" {
			gpuLifecycle.RegisterModel("nemotron", "audiocpp-configs/nemotron.json", 8024, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "nemotron", 1280))
		}
		if strings.TrimSpace(*audiocppBin) != "" && *qwen3ASR06Enabled {
			gpuLifecycle.RegisterModel("qwen3-asr-0.6b", "audiocpp-configs/qwen3-asr-0.6b.json", 8027, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "qwen3-asr-0.6b", 1792))
		}
		if strings.TrimSpace(*audiocppBin) != "" && *qwen3ASR17Enabled {
			gpuLifecycle.RegisterModel("qwen3-asr-1.7b", "audiocpp-configs/qwen3-asr-1.7b.json", 8028, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "qwen3-asr-1.7b", 3584))
		}
		if *fasterWhisperEnabled && strings.TrimSpace(*fasterWhisperPython) != "" && strings.TrimSpace(*fasterWhisperScript) != "" {
			gpuLifecycle.RegisterCommandModel(
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
				modelVRAM(cfg.ModelVRAMMiB, "faster-whisper", 6000),
			)
		}
		if *sttEnabled && *transcribecppEnabled && strings.TrimSpace(*transcribecppBin) != "" && strings.TrimSpace(*transcribecppModel) != "" {
			gpuLifecycle.RegisterCommandModel(
				*transcribecppProviderID,
				*transcribecppBin,
				[]string{
					"--model", *transcribecppModel,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*transcribecppPort),
					"--backend", *transcribecppBackend,
					"--threads", strconv.Itoa(*transcribecppThreads),
				},
				strings.TrimRight(*transcribecppAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, *transcribecppProviderID, 3200),
			)
		}
		if *sttEnabled && *voxtralRealtimeEnabled && strings.TrimSpace(*transcribecppBin) != "" && strings.TrimSpace(*voxtralRealtimeModel) != "" {
			gpuLifecycle.RegisterCommandModel(
				"voxtral-realtime",
				*transcribecppBin,
				[]string{
					"--model", *voxtralRealtimeModel,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*voxtralRealtimePort),
					"--backend", *voxtralRealtimeBackend,
					"--threads", strconv.Itoa(*voxtralRealtimeThreads),
				},
				strings.TrimRight(*voxtralRealtimeAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, "voxtral-realtime", 4800),
			)
		}
	}

	if strings.TrimSpace(*omnivoiceAddr) != "" {
		ov := omnivoice.New(*omnivoiceAddr, nil, encoder)
		if gpuLifecycle != nil {
			ov.StartFunc = func() error { return gpuLifecycle.Start("omnivoice", false) }
		}
		providers = append(providers, ov)
		workers["omnivoice"] = *omnivoiceConcurrency
	}
	if strings.TrimSpace(*supertonicAddr) != "" {
		st := supertonic.New(*supertonicAddr, nil, encoder)
		if gpuLifecycle != nil {
			st.StartFunc = func() error { return gpuLifecycle.Start("supertonic", false) }
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
	idleUnload := map[string]time.Duration{}
	if gpuLifecycle != nil {
		idleDelay := time.Duration(*audiocppIdle) * time.Second
		idleUnload["omnivoice"] = idleDelay
		idleUnload["supertonic"] = idleDelay
	}

	requestTimeout := time.Duration(*requestTimeoutSeconds) * time.Second
	jobQueue := queue.NewManager(queue.Config{
		Providers:         providerMap,
		Workers:           workers,
		IdleUnload:        idleUnload,
		SynthesizeTimeout: requestTimeout,
		RunGate:           gpuRunGate(gpuLifecycle),
	})

	var sttProviders []sttprovider.Provider
	if *sttEnabled && *parakeetEnabled && strings.TrimSpace(*parakeetAddr) != "" {
		pk := parakeet.New(*parakeetAddr, nil)
		if gpuLifecycle != nil {
			pk.StartFunc = func() error { return gpuLifecycle.Start("parakeet", false) }
		}
		sttProviders = append(sttProviders, pk)
	}
	if *fasterWhisperEnabled {
		fw := fasterwhisper.New(*fasterWhisperAddr, nil)
		if gpuLifecycle != nil && strings.TrimSpace(*fasterWhisperPython) != "" && strings.TrimSpace(*fasterWhisperScript) != "" {
			fw.StartFunc = func() error { return gpuLifecycle.Start("faster-whisper", false) }
		}
		sttProviders = append(sttProviders, fw)
	}
	if *sttEnabled && strings.TrimSpace(*nemotronAddr) != "" {
		nm := nemotron.New(*nemotronAddr, nil)
		if gpuLifecycle != nil {
			nm.StartFunc = func() error { return gpuLifecycle.Start("nemotron", false) }
		}
		sttProviders = append(sttProviders, nm)
	}
	if *qwen3ASR06Enabled {
		qw := qwen3asr.New("qwen3-asr-0.6b", "qwen3-asr-0.6b", *qwen3ASR06Addr, nil)
		if gpuLifecycle != nil {
			qw.StartFunc = func() error { return gpuLifecycle.Start("qwen3-asr-0.6b", false) }
		}
		sttProviders = append(sttProviders, qw)
	}
	if *qwen3ASR17Enabled {
		qw := qwen3asr.New("qwen3-asr-1.7b", "qwen3-asr-1.7b", *qwen3ASR17Addr, nil)
		if gpuLifecycle != nil {
			qw.StartFunc = func() error { return gpuLifecycle.Start("qwen3-asr-1.7b", false) }
		}
		sttProviders = append(sttProviders, qw)
	}
	if *sttEnabled && *transcribecppEnabled && strings.TrimSpace(*transcribecppModel) != "" {
		tc := transcribecpp.New(*transcribecppProviderID, *transcribecppAddr, nil)
		if gpuLifecycle != nil && gpuLifecycle.Has(*transcribecppProviderID) {
			tc.StartFunc = func() error { return gpuLifecycle.Start(*transcribecppProviderID, false) }
		}
		sttProviders = append(sttProviders, tc)
	}
	if *sttEnabled && *voxtralRealtimeEnabled && strings.TrimSpace(*voxtralRealtimeModel) != "" {
		vx := transcribecpp.New("voxtral-realtime", *voxtralRealtimeAddr, nil)
		vx.LiveEnabled = true
		vx.LiveAutoLanguage = true
		if gpuLifecycle != nil && gpuLifecycle.Has("voxtral-realtime") {
			vx.StartFunc = func() error { return gpuLifecycle.Start("voxtral-realtime", false) }
		}
		sttProviders = append(sttProviders, vx)
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
		SttRunGate:         gpuRunGate(gpuLifecycle),
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

func configPathFromArgs(args []string, fallback string) string {
	for i, arg := range args {
		switch {
		case (arg == "-config" || arg == "--config") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(arg, "-config="):
			return strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			return strings.TrimPrefix(arg, "--config=")
		}
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

func vramLimitString(value any) string {
	if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" {
		return "auto"
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func resolveVRAMLimit(value string) (int, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "auto" {
		return lifecycle.DetectVRAMMiB()
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		return 0, fmt.Errorf("max_vram_mib must be auto or a positive integer, got %q", value)
	}
	return limit, nil
}

func modelVRAM(configured map[string]int, id string, fallback int) int {
	if value := configured[id]; value > 0 {
		return value
	}
	return fallback
}

func gpuRunGate(manager *lifecycle.Manager) func(string) (func(), error) {
	if manager == nil {
		return nil
	}
	return func(providerID string) (func(), error) {
		if !manager.Has(providerID) {
			return nil, nil
		}
		return manager.Acquire(providerID)
	}
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
