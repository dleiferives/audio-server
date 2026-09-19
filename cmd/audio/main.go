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

	"github.com/dleiferives/audio-server/internal/analysisjob"
	"github.com/dleiferives/audio-server/internal/analysisprovider"
	"github.com/dleiferives/audio-server/internal/encode"
	"github.com/dleiferives/audio-server/internal/lifecycle"
	"github.com/dleiferives/audio-server/internal/provider"
	"github.com/dleiferives/audio-server/internal/provider/align"
	"github.com/dleiferives/audio-server/internal/provider/bsroformer"
	"github.com/dleiferives/audio-server/internal/provider/chatterbox"
	"github.com/dleiferives/audio-server/internal/provider/cosyvoice"
	"github.com/dleiferives/audio-server/internal/provider/espeak"
	"github.com/dleiferives/audio-server/internal/provider/fasterwhisper"
	"github.com/dleiferives/audio-server/internal/provider/htdemucs"
	"github.com/dleiferives/audio-server/internal/provider/htdemucsdnr"
	"github.com/dleiferives/audio-server/internal/provider/kokoro"
	"github.com/dleiferives/audio-server/internal/provider/moonshine"
	"github.com/dleiferives/audio-server/internal/provider/nemotron"
	"github.com/dleiferives/audio-server/internal/provider/omnivoice"
	"github.com/dleiferives/audio-server/internal/provider/parakeet"
	"github.com/dleiferives/audio-server/internal/provider/pyannote"
	"github.com/dleiferives/audio-server/internal/provider/qwen3asr"
	"github.com/dleiferives/audio-server/internal/provider/sortformer"
	"github.com/dleiferives/audio-server/internal/provider/supertonic"
	"github.com/dleiferives/audio-server/internal/provider/transcribecpp"
	"github.com/dleiferives/audio-server/internal/provider/wespeaker"
	"github.com/dleiferives/audio-server/internal/queue"
	"github.com/dleiferives/audio-server/internal/segment"
	"github.com/dleiferives/audio-server/internal/separationjob"
	"github.com/dleiferives/audio-server/internal/server"
	"github.com/dleiferives/audio-server/internal/store"
	"github.com/dleiferives/audio-server/internal/sttcorpus"
	"github.com/dleiferives/audio-server/internal/sttprovider"

	yaml "gopkg.in/yaml.v3"
)

type configFile struct {
	Addr                      string         `yaml:"addr"`
	APIKey                    string         `yaml:"api_key"`
	WebDir                    string         `yaml:"web_dir"`
	AudioStoreDir             string         `yaml:"audio_store_dir"`
	AudioTTLSeconds           int            `yaml:"audio_ttl_seconds"`
	SttCaptureEnabled         bool           `yaml:"stt_capture_enabled"`
	SttCaptureDir             string         `yaml:"stt_capture_dir"`
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
	OmnivoiceMaxWords         int            `yaml:"omnivoice_max_words"`
	OmnivoiceTargetWords      int            `yaml:"omnivoice_target_words"`
	SegmentPython             string         `yaml:"segment_python"`
	SegmentScript             string         `yaml:"segment_script"`
	SupertonicAddr            string         `yaml:"supertonic_addr"`
	AudiocppBin               string         `yaml:"audiocpp_bin"`
	AudiocppIdleUnloadSeconds int            `yaml:"audiocpp_idle_unload_seconds"`
	MaxVRAMMiB                any            `yaml:"max_vram_mib"`
	ModelVRAMMiB              map[string]int `yaml:"model_vram_mib"`
	KokoroEnabled             bool           `yaml:"kokoro_enabled"`
	ChatterboxEnabled         bool           `yaml:"chatterbox_enabled"`
	ChatterboxAddr            string         `yaml:"chatterbox_addr"`
	ChatterboxPort            int            `yaml:"chatterbox_port"`
	ChatterboxPython          string         `yaml:"chatterbox_python"`
	ChatterboxScript          string         `yaml:"chatterbox_script"`
	ChatterboxDevice          string         `yaml:"chatterbox_device"`
	CosyvoiceEnabled          bool           `yaml:"cosyvoice_enabled"`
	CosyvoiceAddr             string         `yaml:"cosyvoice_addr"`
	CosyvoicePort             int            `yaml:"cosyvoice_port"`
	CosyvoicePython           string         `yaml:"cosyvoice_python"`
	CosyvoiceScript           string         `yaml:"cosyvoice_script"`
	CosyvoiceRepoPath         string         `yaml:"cosyvoice_repo_path"`
	CosyvoiceModelDir         string         `yaml:"cosyvoice_model_dir"`
	CosyvoiceDevice           string         `yaml:"cosyvoice_device"`
	FasterWhisperEnabled      bool           `yaml:"faster_whisper_enabled"`
	FasterWhisperAddr         string         `yaml:"faster_whisper_addr"`
	FasterWhisperPython       string         `yaml:"faster_whisper_python"`
	FasterWhisperScript       string         `yaml:"faster_whisper_script"`
	FasterWhisperPort         int            `yaml:"faster_whisper_port"`
	FasterWhisperModelSize    string         `yaml:"faster_whisper_model_size"`
	FasterWhisperDevice       string         `yaml:"faster_whisper_device"`
	FasterWhisperComputeType  string         `yaml:"faster_whisper_compute_type"`
	MoonshineEnabled          bool           `yaml:"moonshine_enabled"`
	MoonshineAddr             string         `yaml:"moonshine_addr"`
	MoonshinePort             int            `yaml:"moonshine_port"`
	MoonshinePython           string         `yaml:"moonshine_python"`
	MoonshineScript           string         `yaml:"moonshine_script"`
	MoonshineLanguage         string         `yaml:"moonshine_language"`
	MoonshineModelArch        string         `yaml:"moonshine_model_arch"`
	MoonshineUpdateInterval   float64        `yaml:"moonshine_update_interval"`
	MoonshineThreads          int            `yaml:"moonshine_threads"`
	MoonshineIdleUnload       *int           `yaml:"moonshine_idle_unload_seconds"`
	MoonshinePreload          *bool          `yaml:"moonshine_preload"`
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
	AnalysisEnabled           bool           `yaml:"analysis_enabled"`
	AnalysisMaxUploadMiB      int            `yaml:"analysis_max_upload_mib"`
	SortformerAddr            string         `yaml:"sortformer_addr"`
	SortformerPort            int            `yaml:"sortformer_port"`
	BSRoformerAddr            string         `yaml:"bs_roformer_addr"`
	BSRoformerPort            int            `yaml:"bs_roformer_port"`
	HTDemucsAddr              string         `yaml:"htdemucs_addr"`
	HTDemucsPort              int            `yaml:"htdemucs_port"`
	DefaultAnalysisSeparator  string         `yaml:"default_analysis_separator"`
	WespeakerEnabled          bool           `yaml:"wespeaker_enabled"`
	WespeakerAddr             string         `yaml:"wespeaker_addr"`
	WespeakerPort             int            `yaml:"wespeaker_port"`
	WespeakerBin              string         `yaml:"wespeaker_bin"`
	WespeakerModel            string         `yaml:"wespeaker_model"`
	PyannoteEnabled           bool           `yaml:"pyannote_enabled"`
	PyannoteAddr              string         `yaml:"pyannote_addr"`
	PyannotePort              int            `yaml:"pyannote_port"`
	PyannotePython            string         `yaml:"pyannote_python"`
	PyannoteScript            string         `yaml:"pyannote_script"`
	PyannoteModel             string         `yaml:"pyannote_model"`
	PyannoteDevice            string         `yaml:"pyannote_device"`
	DefaultAnalysisDiarizer   string         `yaml:"default_analysis_diarizer"`
	HTDemucsDnREnabled        bool           `yaml:"htdemucs_dnr_enabled"`
	HTDemucsDnRAddr           string         `yaml:"htdemucs_dnr_addr"`
	HTDemucsDnRPort           int            `yaml:"htdemucs_dnr_port"`
	HTDemucsDnRPython         string         `yaml:"htdemucs_dnr_python"`
	HTDemucsDnRScript         string         `yaml:"htdemucs_dnr_script"`
	HTDemucsDnRCheckpoint     string         `yaml:"htdemucs_dnr_checkpoint"`
	HTDemucsDnRDevice         string         `yaml:"htdemucs_dnr_device"`
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
	omnivoiceMaxWords := flag.Int("omnivoice-max-words", cfg.OmnivoiceMaxWords, "hard cap on words per OmniVoice synthesis chunk; 0 disables auto-segmentation")
	omnivoiceTargetWords := flag.Int("omnivoice-target-words", cfg.OmnivoiceTargetWords, "preferred words per OmniVoice chunk; 0 means half of -omnivoice-max-words")
	segmentPython := flag.String("segment-python", env("AUDIO_SEGMENT_PYTHON", valueOr(cfg.SegmentPython, "python3")), "python interpreter for the sentence segmenter")
	segmentScript := flag.String("segment-script", valueOr(cfg.SegmentScript, "tts/segment/segment.py"), "path to the sentence segmenter script")
	supertonicAddr := flag.String("supertonic-addr", env("AUDIO_SUPERTONIC_ADDR", cfg.SupertonicAddr), "Supertonic sidecar base URL (e.g. http://127.0.0.1:8022); disabled when blank")
	audiocppBin := flag.String("audiocpp-bin", env("AUDIO_AUDIOCPP_BIN", cfg.AudiocppBin), "path to audiocpp_server binary")
	audiocppIdle := flag.Int("audiocpp-idle-unload-seconds", envInt("AUDIO_AUDIOCPP_IDLE_UNLOAD", cfg.AudiocppIdleUnloadSeconds), "seconds before unloading idle audiocpp_server instances")
	maxVRAM := flag.String("max-vram-mib", env("AUDIO_MAX_VRAM_MIB", vramLimitString(cfg.MaxVRAMMiB)), "GPU residency budget in MiB, or auto")
	kokoroEnabled := flag.Bool("kokoro-enabled", cfg.KokoroEnabled, "enable Kokoro TTS provider")
	chatterboxEnabled := flag.Bool("chatterbox-enabled", cfg.ChatterboxEnabled, "enable Chatterbox voice-cloning TTS provider")
	chatterboxAddr := flag.String("chatterbox-addr", env("AUDIO_CHATTERBOX_ADDR", valueOr(cfg.ChatterboxAddr, "http://127.0.0.1:8037")), "Chatterbox sidecar base URL")
	chatterboxPort := flag.Int("chatterbox-port", envInt("AUDIO_CHATTERBOX_PORT", intOr(cfg.ChatterboxPort, 8037)), "local Chatterbox sidecar port")
	chatterboxPython := flag.String("chatterbox-python", env("AUDIO_CHATTERBOX_PYTHON", valueOr(cfg.ChatterboxPython, "python3")), "Python executable for the Chatterbox sidecar")
	chatterboxScript := flag.String("chatterbox-script", env("AUDIO_CHATTERBOX_SCRIPT", valueOr(cfg.ChatterboxScript, "tts/chatterbox/server.py")), "path to the Chatterbox sidecar script")
	chatterboxDevice := flag.String("chatterbox-device", env("AUDIO_CHATTERBOX_DEVICE", valueOr(cfg.ChatterboxDevice, "auto")), "Chatterbox inference device")
	cosyvoiceEnabled := flag.Bool("cosyvoice-enabled", cfg.CosyvoiceEnabled, "enable CosyVoice2 voice-cloning TTS provider")
	cosyvoiceAddr := flag.String("cosyvoice-addr", env("AUDIO_COSYVOICE_ADDR", valueOr(cfg.CosyvoiceAddr, "http://127.0.0.1:8038")), "CosyVoice2 sidecar base URL")
	cosyvoicePort := flag.Int("cosyvoice-port", envInt("AUDIO_COSYVOICE_PORT", intOr(cfg.CosyvoicePort, 8038)), "local CosyVoice2 sidecar port")
	cosyvoicePython := flag.String("cosyvoice-python", env("AUDIO_COSYVOICE_PYTHON", valueOr(cfg.CosyvoicePython, "python3")), "Python executable for the CosyVoice2 sidecar")
	cosyvoiceScript := flag.String("cosyvoice-script", env("AUDIO_COSYVOICE_SCRIPT", valueOr(cfg.CosyvoiceScript, "tts/cosyvoice/server.py")), "path to the CosyVoice2 sidecar script")
	cosyvoiceRepoPath := flag.String("cosyvoice-repo-path", env("AUDIO_COSYVOICE_REPO_PATH", cfg.CosyvoiceRepoPath), "path to a FunAudioLLM/CosyVoice checkout (put on PYTHONPATH)")
	cosyvoiceModelDir := flag.String("cosyvoice-model-dir", env("AUDIO_COSYVOICE_MODEL_DIR", valueOr(cfg.CosyvoiceModelDir, "FunAudioLLM/CosyVoice2-0.5B")), "CosyVoice2 model id or local directory")
	cosyvoiceDevice := flag.String("cosyvoice-device", env("AUDIO_COSYVOICE_DEVICE", valueOr(cfg.CosyvoiceDevice, "auto")), "CosyVoice2 inference device")
	fasterWhisperEnabled := flag.Bool("faster-whisper-enabled", cfg.FasterWhisperEnabled, "enable faster-whisper STT provider")
	fasterWhisperAddr := flag.String("faster-whisper-addr", env("AUDIO_FASTERWHISPER_ADDR", valueOr(cfg.FasterWhisperAddr, "http://127.0.0.1:8030")), "faster-whisper sidecar base URL")
	fasterWhisperPython := flag.String("faster-whisper-python", env("AUDIO_FASTERWHISPER_PYTHON", valueOr(cfg.FasterWhisperPython, "python3")), "Python executable for the faster-whisper sidecar")
	fasterWhisperScript := flag.String("faster-whisper-script", env("AUDIO_FASTERWHISPER_SCRIPT", valueOr(cfg.FasterWhisperScript, "stt/fasterwhisper/server.py")), "path to the faster-whisper sidecar script")
	fasterWhisperPort := flag.Int("faster-whisper-port", envInt("AUDIO_FASTERWHISPER_PORT", intOr(cfg.FasterWhisperPort, 8030)), "local faster-whisper sidecar port")
	fasterWhisperModelSize := flag.String("faster-whisper-model-size", env("AUDIO_FASTERWHISPER_MODEL_SIZE", valueOr(cfg.FasterWhisperModelSize, "small")), "faster-whisper model size")
	fasterWhisperDevice := flag.String("faster-whisper-device", env("AUDIO_FASTERWHISPER_DEVICE", valueOr(cfg.FasterWhisperDevice, "auto")), "faster-whisper inference device")
	fasterWhisperComputeType := flag.String("faster-whisper-compute-type", env("AUDIO_FASTERWHISPER_COMPUTE_TYPE", valueOr(cfg.FasterWhisperComputeType, "default")), "faster-whisper compute type")
	moonshineEnabled := flag.Bool("moonshine-enabled", cfg.MoonshineEnabled, "enable the CPU-only Moonshine v2 streaming STT provider")
	moonshineAddr := flag.String("moonshine-addr", env("AUDIO_MOONSHINE_ADDR", valueOr(cfg.MoonshineAddr, "http://127.0.0.1:8042")), "Moonshine sidecar base URL")
	moonshinePort := flag.Int("moonshine-port", envInt("AUDIO_MOONSHINE_PORT", intOr(cfg.MoonshinePort, 8042)), "local Moonshine sidecar port")
	moonshinePython := flag.String("moonshine-python", env("AUDIO_MOONSHINE_PYTHON", valueOr(cfg.MoonshinePython, "python3")), "Python executable for the Moonshine sidecar")
	moonshineScript := flag.String("moonshine-script", env("AUDIO_MOONSHINE_SCRIPT", valueOr(cfg.MoonshineScript, "stt/moonshine/server.py")), "path to the Moonshine sidecar script")
	moonshineLanguage := flag.String("moonshine-language", env("AUDIO_MOONSHINE_LANGUAGE", valueOr(cfg.MoonshineLanguage, "en")), "default Moonshine language; one model is published per language")
	moonshineModelArch := flag.String("moonshine-model-arch", env("AUDIO_MOONSHINE_MODEL_ARCH", valueOr(cfg.MoonshineModelArch, "MEDIUM_STREAMING")), "Moonshine ModelArch name (MEDIUM_STREAMING, SMALL_STREAMING, TINY_STREAMING)")
	moonshineUpdateInterval := flag.Float64("moonshine-update-interval", cfg.MoonshineUpdateInterval, "seconds between Moonshine streaming transcript refreshes")
	moonshineThreads := flag.Int("moonshine-threads", envInt("AUDIO_MOONSHINE_THREADS", cfg.MoonshineThreads), "ONNX Runtime intra-op threads for Moonshine; 0 uses all physical cores")
	// Default to keeping the model resident and warming it at startup. Moonshine
	// costs no VRAM, so the only price is RAM, and reloading it costs about a
	// second on the first utterance after a pause.
	moonshineIdleUnload := flag.Int("moonshine-idle-unload-seconds", intFromPtr(cfg.MoonshineIdleUnload, 0), "unload the Moonshine model after this many idle seconds; 0 keeps it resident")
	moonshinePreload := flag.Bool("moonshine-preload", boolFromPtr(cfg.MoonshinePreload, true), "load the Moonshine model at sidecar startup instead of on first request")
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
	analysisEnabled := flag.Bool("analysis-enabled", cfg.AnalysisEnabled, "enable asynchronous diarization and speaker-embedding jobs")
	analysisMaxUploadMiB := flag.Int("analysis-max-upload-mib", envInt("AUDIO_ANALYSIS_MAX_UPLOAD_MIB", intOr(cfg.AnalysisMaxUploadMiB, 2048)), "maximum uploaded audio/video size for an analysis job")
	sortformerAddr := flag.String("sortformer-addr", env("AUDIO_SORTFORMER_ADDR", valueOr(cfg.SortformerAddr, "http://127.0.0.1:8033")), "Sortformer audio.cpp sidecar base URL")
	sortformerPort := flag.Int("sortformer-port", envInt("AUDIO_SORTFORMER_PORT", intOr(cfg.SortformerPort, 8033)), "local Sortformer sidecar port")
	bsRoformerAddr := flag.String("bs-roformer-addr", env("AUDIO_BS_ROFORMER_ADDR", valueOr(cfg.BSRoformerAddr, "http://127.0.0.1:8035")), "BS-RoFormer audio.cpp sidecar base URL")
	bsRoformerPort := flag.Int("bs-roformer-port", envInt("AUDIO_BS_ROFORMER_PORT", intOr(cfg.BSRoformerPort, 8035)), "local BS-RoFormer sidecar port")
	htdemucsAddr := flag.String("htdemucs-addr", env("AUDIO_HTDEMUCS_ADDR", valueOr(cfg.HTDemucsAddr, "http://127.0.0.1:8039")), "HTDemucs audio.cpp sidecar base URL")
	htdemucsPort := flag.Int("htdemucs-port", envInt("AUDIO_HTDEMUCS_PORT", intOr(cfg.HTDemucsPort, 8039)), "local HTDemucs sidecar port")
	defaultAnalysisSeparator := flag.String("default-analysis-separator", env("AUDIO_ANALYSIS_SEP_PROVIDER", valueOr(cfg.DefaultAnalysisSeparator, "bs-roformer")), "default dialogue-separation model for analysis/separation jobs")
	wespeakerEnabled := flag.Bool("wespeaker-enabled", cfg.WespeakerEnabled, "enable native WeSpeaker speaker embeddings")
	wespeakerAddr := flag.String("wespeaker-addr", env("AUDIO_WESPEAKER_ADDR", valueOr(cfg.WespeakerAddr, "http://127.0.0.1:8034")), "WeSpeaker sidecar base URL")
	wespeakerPort := flag.Int("wespeaker-port", envInt("AUDIO_WESPEAKER_PORT", intOr(cfg.WespeakerPort, 8034)), "local WeSpeaker sidecar port")
	wespeakerBin := flag.String("wespeaker-bin", env("AUDIO_WESPEAKER_BIN", valueOr(cfg.WespeakerBin, "bin/wespeaker_server")), "path to native WeSpeaker sidecar")
	wespeakerModel := flag.String("wespeaker-model", env("AUDIO_WESPEAKER_MODEL", cfg.WespeakerModel), "path to WeSpeaker ONNX speaker model")
	pyannoteEnabled := flag.Bool("pyannote-enabled", cfg.PyannoteEnabled, "enable pyannote.audio diarization provider")
	pyannoteAddr := flag.String("pyannote-addr", env("AUDIO_PYANNOTE_ADDR", valueOr(cfg.PyannoteAddr, "http://127.0.0.1:8036")), "pyannote sidecar base URL")
	pyannotePort := flag.Int("pyannote-port", envInt("AUDIO_PYANNOTE_PORT", intOr(cfg.PyannotePort, 8036)), "local pyannote sidecar port")
	pyannotePython := flag.String("pyannote-python", env("AUDIO_PYANNOTE_PYTHON", valueOr(cfg.PyannotePython, "python3")), "Python executable for the pyannote sidecar")
	pyannoteScript := flag.String("pyannote-script", env("AUDIO_PYANNOTE_SCRIPT", valueOr(cfg.PyannoteScript, "speaker/pyannote/server.py")), "path to the pyannote sidecar script")
	pyannoteModel := flag.String("pyannote-model", env("AUDIO_PYANNOTE_MODEL", valueOr(cfg.PyannoteModel, "pyannote/speaker-diarization-community-1")), "pyannote pipeline model id")
	pyannoteDevice := flag.String("pyannote-device", env("AUDIO_PYANNOTE_DEVICE", valueOr(cfg.PyannoteDevice, "auto")), "pyannote inference device")
	defaultAnalysisDiarizer := flag.String("default-analysis-diarizer", env("AUDIO_ANALYSIS_DIAR_PROVIDER", valueOr(cfg.DefaultAnalysisDiarizer, "sortformer")), "default diarization model for analysis jobs")
	htdemucsDnREnabled := flag.Bool("htdemucs-dnr-enabled", cfg.HTDemucsDnREnabled, "enable the DnR-trained cinematic (dialogue/music/effects) HTDemucs separator")
	htdemucsDnRAddr := flag.String("htdemucs-dnr-addr", env("AUDIO_HTDEMUCS_DNR_ADDR", valueOr(cfg.HTDemucsDnRAddr, "http://127.0.0.1:8041")), "HTDemucs-DnR sidecar base URL")
	htdemucsDnRPort := flag.Int("htdemucs-dnr-port", envInt("AUDIO_HTDEMUCS_DNR_PORT", intOr(cfg.HTDemucsDnRPort, 8041)), "local HTDemucs-DnR sidecar port")
	htdemucsDnRPython := flag.String("htdemucs-dnr-python", env("AUDIO_HTDEMUCS_DNR_PYTHON", valueOr(cfg.HTDemucsDnRPython, "python3")), "Python executable for the HTDemucs-DnR sidecar")
	htdemucsDnRScript := flag.String("htdemucs-dnr-script", env("AUDIO_HTDEMUCS_DNR_SCRIPT", valueOr(cfg.HTDemucsDnRScript, "separation/htdemucs-dnr/server.py")), "path to the HTDemucs-DnR sidecar script")
	htdemucsDnRCheckpoint := flag.String("htdemucs-dnr-checkpoint", env("AUDIO_HTDEMUCS_DNR_CHECKPOINT", cfg.HTDemucsDnRCheckpoint), "path to the HTDemucs-DnR .th checkpoint (blank = sidecar default)")
	htdemucsDnRDevice := flag.String("htdemucs-dnr-device", env("AUDIO_HTDEMUCS_DNR_DEVICE", valueOr(cfg.HTDemucsDnRDevice, "auto")), "HTDemucs-DnR inference device")
	webDir := flag.String("web-dir", env("AUDIO_WEB_DIR", cfg.WebDir), "optional path to static web frontend directory")
	audioTTL := flag.Int("audio-ttl-seconds", envInt("AUDIO_AUDIO_TTL_SECONDS", cfg.AudioTTLSeconds), "audio file retention in seconds (0 = forever)")
	audioStoreDir := flag.String("audio-store-dir", env("AUDIO_STORE_DIR", cfg.AudioStoreDir), "directory for generated audio files (empty = in-memory)")
	sttCaptureEnabled := flag.Bool("stt-capture-enabled", cfg.SttCaptureEnabled, "record transcribed audio and text as an ASR fine-tuning corpus")
	sttCaptureDir := flag.String("stt-capture-dir", env("AUDIO_STT_CAPTURE_DIR", valueOr(cfg.SttCaptureDir, "stt-corpus")), "directory for the STT fine-tuning corpus; never swept on a TTL")
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
	if strings.TrimSpace(*audiocppBin) != "" || *fasterWhisperEnabled || *transcribecppEnabled || *voxtralRealtimeEnabled || *analysisEnabled || *chatterboxEnabled || *cosyvoiceEnabled || *moonshineEnabled {
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
		if *analysisEnabled && strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*sortformerAddr) != "" {
			gpuLifecycle.RegisterModel("sortformer", "audiocpp-configs/sortformer.json", *sortformerPort, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "sortformer", 512))
		}
		if *analysisEnabled && strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*bsRoformerAddr) != "" {
			gpuLifecycle.RegisterModel("bs-roformer", "audiocpp-configs/bs-roformer.json", *bsRoformerPort, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "bs-roformer", 2048))
		}
		if *analysisEnabled && strings.TrimSpace(*audiocppBin) != "" && strings.TrimSpace(*htdemucsAddr) != "" {
			gpuLifecycle.RegisterModel("htdemucs", "audiocpp-configs/htdemucs.json", *htdemucsPort, idleDelay, modelVRAM(cfg.ModelVRAMMiB, "htdemucs", 2048))
		}
		if *analysisEnabled && *wespeakerEnabled && strings.TrimSpace(*wespeakerBin) != "" && strings.TrimSpace(*wespeakerModel) != "" {
			gpuLifecycle.RegisterCommandModel(
				"wespeaker", *wespeakerBin,
				[]string{"--model", *wespeakerModel, "--host", "127.0.0.1", "--port", strconv.Itoa(*wespeakerPort)},
				strings.TrimRight(*wespeakerAddr, "/")+"/health", 0, 0,
			)
		}
		if *analysisEnabled && *pyannoteEnabled && strings.TrimSpace(*pyannotePython) != "" && strings.TrimSpace(*pyannoteScript) != "" {
			gpuLifecycle.RegisterCommandModel(
				"pyannote", *pyannotePython,
				[]string{
					*pyannoteScript,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*pyannotePort),
					"--model-id", *pyannoteModel,
					"--device", *pyannoteDevice,
				},
				strings.TrimRight(*pyannoteAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, "pyannote", 1536),
			)
		}
		if *analysisEnabled && *htdemucsDnREnabled && strings.TrimSpace(*htdemucsDnRPython) != "" && strings.TrimSpace(*htdemucsDnRScript) != "" {
			args := []string{
				*htdemucsDnRScript,
				"--host", "127.0.0.1",
				"--port", strconv.Itoa(*htdemucsDnRPort),
				"--device", *htdemucsDnRDevice,
			}
			if strings.TrimSpace(*htdemucsDnRCheckpoint) != "" {
				args = append(args, "--checkpoint", *htdemucsDnRCheckpoint)
			}
			gpuLifecycle.RegisterCommandModel(
				"htdemucs-dnr", *htdemucsDnRPython, args,
				strings.TrimRight(*htdemucsDnRAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, "htdemucs-dnr", 2560),
			)
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
		if *moonshineEnabled && strings.TrimSpace(*moonshinePython) != "" && strings.TrimSpace(*moonshineScript) != "" {
			args := []string{
				*moonshineScript,
				"--host", "127.0.0.1",
				"--port", strconv.Itoa(*moonshinePort),
				"--language", *moonshineLanguage,
				"--model-arch", *moonshineModelArch,
			}
			if *moonshineUpdateInterval > 0 {
				args = append(args, "--update-interval", strconv.FormatFloat(*moonshineUpdateInterval, 'f', -1, 64))
			}
			if *moonshineThreads > 0 {
				args = append(args, "--threads", strconv.Itoa(*moonshineThreads))
			}
			args = append(args, "--idle-unload-seconds", strconv.Itoa(*moonshineIdleUnload))
			if *moonshinePreload {
				args = append(args, "--preload")
			}
			// Moonshine runs on the CPU, so it is registered with a zero VRAM
			// cost: it never counts against the residency budget and is never
			// evicted to make room for a GPU model. The sidecar runs its own
			// idle-unload timer instead of being driven from here.
			gpuLifecycle.RegisterCommandModel(
				"moonshine", *moonshinePython, args,
				strings.TrimRight(*moonshineAddr, "/")+"/health", 0, 0,
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
		if *chatterboxEnabled && strings.TrimSpace(*chatterboxPython) != "" && strings.TrimSpace(*chatterboxScript) != "" {
			gpuLifecycle.RegisterCommandModel(
				"chatterbox", *chatterboxPython,
				[]string{
					*chatterboxScript,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*chatterboxPort),
					"--device", *chatterboxDevice,
				},
				strings.TrimRight(*chatterboxAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, "chatterbox", 3072),
			)
		}
		if *cosyvoiceEnabled && strings.TrimSpace(*cosyvoicePython) != "" && strings.TrimSpace(*cosyvoiceScript) != "" {
			gpuLifecycle.RegisterCommandModel(
				"cosyvoice", *cosyvoicePython,
				[]string{
					*cosyvoiceScript,
					"--host", "127.0.0.1",
					"--port", strconv.Itoa(*cosyvoicePort),
					"--model-dir", *cosyvoiceModelDir,
					"--device", *cosyvoiceDevice,
					"--repo-path", *cosyvoiceRepoPath,
				},
				strings.TrimRight(*cosyvoiceAddr, "/")+"/health",
				idleDelay,
				modelVRAM(cfg.ModelVRAMMiB, "cosyvoice", 4096),
			)
		}
	}

	if strings.TrimSpace(*omnivoiceAddr) != "" {
		ov := omnivoice.New(*omnivoiceAddr, nil, encoder)
		if *omnivoiceMaxWords > 0 {
			ov.Segmenter = segment.Python{Bin: *segmentPython, Script: *segmentScript}
			ov.MaxWords = *omnivoiceMaxWords
			ov.TargetWords = *omnivoiceTargetWords
			log.Printf("omnivoice: auto-segmentation on (max %d words, target %d)", ov.MaxWords, ov.TargetWords)
		}
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
	if *chatterboxEnabled && strings.TrimSpace(*chatterboxAddr) != "" {
		cb := chatterbox.New(*chatterboxAddr, nil, encoder)
		if gpuLifecycle != nil && gpuLifecycle.Has("chatterbox") {
			cb.StartFunc = func() error { return gpuLifecycle.Start("chatterbox", false) }
		}
		providers = append(providers, cb)
		workers["chatterbox"] = 1
	}
	if *cosyvoiceEnabled && strings.TrimSpace(*cosyvoiceAddr) != "" {
		cv := cosyvoice.New(*cosyvoiceAddr, nil, encoder)
		if gpuLifecycle != nil && gpuLifecycle.Has("cosyvoice") {
			cv.StartFunc = func() error { return gpuLifecycle.Start("cosyvoice", false) }
		}
		providers = append(providers, cv)
		workers["cosyvoice"] = 1
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
		if *chatterboxEnabled {
			idleUnload["chatterbox"] = idleDelay
		}
		if *cosyvoiceEnabled {
			idleUnload["cosyvoice"] = idleDelay
		}
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
	if *moonshineEnabled {
		ms := moonshine.New(*moonshineAddr, nil)
		if gpuLifecycle != nil && gpuLifecycle.Has("moonshine") {
			ms.StartFunc = func() error { return gpuLifecycle.Start("moonshine", false) }
		}
		sttProviders = append(sttProviders, ms)
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

	// Start the CPU sidecar eagerly rather than on the first request. Otherwise
	// --preload cannot help: the process would not exist until someone spoke, so
	// the first utterance after boot pays python startup plus a model load. It
	// costs no VRAM and an idle resident model uses no CPU, so there is nothing
	// to save by waiting. Done in the background so a slow load cannot delay the
	// listener coming up.
	if *moonshineEnabled && *moonshinePreload && gpuLifecycle != nil && gpuLifecycle.Has("moonshine") {
		go func() {
			if err := gpuLifecycle.Start("moonshine", false); err != nil {
				log.Printf("moonshine: warm start failed, will start on first request: %v", err)
				return
			}
			log.Printf("moonshine: sidecar warm and resident")
		}()
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

	// Deliberately not bound to audio_ttl_seconds: the TTL sweeps generated TTS
	// output, while this is a training corpus that must never expire.
	var sttCorpus *sttcorpus.Recorder
	if *sttCaptureEnabled && strings.TrimSpace(*sttCaptureDir) != "" {
		c, err := sttcorpus.New(*sttCaptureDir)
		if err != nil {
			log.Fatalf("stt corpus: %v", err)
		}
		sttCorpus = c
		log.Printf("stt corpus: recording transcribed audio to %s (no expiry)", c.Dir())
	}

	var alignProvider server.Aligner
	if *alignEnabled {
		langMap := loadAlignLanguages(*alignMFAModelsConfig)
		alignProvider = align.NewAlignProvider(*alignMFAEnv, *alignMFAWorkDir, "", langMap)
		log.Printf("alignment: enabled (%d languages)", len(langMap))
	}

	var analysisJobs *analysisjob.Manager
	var separationJobs *separationjob.Manager
	if *analysisEnabled {
		diarizers := make(map[string]analysisprovider.Diarizer)
		if strings.TrimSpace(*sortformerAddr) != "" {
			diarizer := sortformer.New(*sortformerAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("sortformer") {
				diarizer.StartFunc = func() error { return gpuLifecycle.Start("sortformer", false) }
			}
			diarizers[sortformer.ID] = diarizer
		}
		if *pyannoteEnabled && strings.TrimSpace(*pyannoteAddr) != "" {
			diarizer := pyannote.New(*pyannoteAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("pyannote") {
				diarizer.StartFunc = func() error { return gpuLifecycle.Start("pyannote", false) }
			}
			diarizers[pyannote.ID] = diarizer
		}
		defaultDiarizer := *defaultAnalysisDiarizer
		if _, ok := diarizers[defaultDiarizer]; !ok {
			for id := range diarizers {
				defaultDiarizer = id
				break
			}
		}
		separators := make(map[string]analysisprovider.Separator)
		if strings.TrimSpace(*bsRoformerAddr) != "" {
			separator := bsroformer.New(*bsRoformerAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("bs-roformer") {
				separator.StartFunc = func() error { return gpuLifecycle.Start("bs-roformer", false) }
			}
			separators[bsroformer.ID] = separator
		}
		if strings.TrimSpace(*htdemucsAddr) != "" {
			separator := htdemucs.New(*htdemucsAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("htdemucs") {
				separator.StartFunc = func() error { return gpuLifecycle.Start("htdemucs", false) }
			}
			separators[htdemucs.ID] = separator
		}
		if *htdemucsDnREnabled && strings.TrimSpace(*htdemucsDnRAddr) != "" {
			separator := htdemucsdnr.New(*htdemucsDnRAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("htdemucs-dnr") {
				separator.StartFunc = func() error { return gpuLifecycle.Start("htdemucs-dnr", false) }
			}
			separators[htdemucsdnr.ID] = separator
		}
		defaultSeparator := *defaultAnalysisSeparator
		if _, ok := separators[defaultSeparator]; !ok {
			for id := range separators {
				defaultSeparator = id
				break
			}
		}
		var embedder analysisprovider.Embedder
		if *wespeakerEnabled {
			provider := wespeaker.New(*wespeakerAddr, nil)
			if gpuLifecycle != nil && gpuLifecycle.Has("wespeaker") {
				provider.StartFunc = func() error { return gpuLifecycle.Start("wespeaker", false) }
			}
			embedder = provider
		}
		transcribers := make(map[string]sttprovider.Provider, len(sttProviders))
		for _, provider := range sttProviders {
			transcribers[provider.ID()] = provider
		}
		analysisJobs = analysisjob.New(analysisjob.Config{
			Diarizers: diarizers, DefaultDiarizer: defaultDiarizer,
			Embedder: embedder, Separators: separators, DefaultSeparator: defaultSeparator, AudioNormalizer: encoder,
			Transcribers: transcribers, DefaultTranscriber: *defaultSttProvider,
			RunGate: gpuRunGate(gpuLifecycle), Timeout: requestTimeout,
		})
		if audioStore != nil {
			separationJobs = separationjob.New(separationjob.Config{
				Separators: separators, DefaultSeparator: defaultSeparator, AudioNormalizer: encoder,
				Store: audioStore, RunGate: gpuRunGate(gpuLifecycle), Timeout: requestTimeout,
			})
		}
	}

	audioServer, err := server.New(server.Config{
		Providers:              providers,
		DefaultProvider:        *defaultProvider,
		APIKey:                 *apiKey,
		MaxInputChars:          *maxInputChars,
		RequestTimeout:         requestTimeout,
		Queue:                  jobQueue,
		StreamWorkers:          workers,
		SttProviders:           sttProviders,
		DefaultSttProvider:     *defaultSttProvider,
		SttAudioNormalizer:     encoder,
		SttRunGate:             gpuRunGate(gpuLifecycle),
		WebDir:                 *webDir,
		AudioStore:             audioStore,
		SttCorpus:              sttCorpus,
		AlignProvider:          alignProvider,
		AnalysisJobs:           analysisJobs,
		AnalysisMaxUploadBytes: int64(*analysisMaxUploadMiB) << 20,
		SeparationJobs:         separationJobs,
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

// intFromPtr and boolFromPtr distinguish "absent from the config" from an
// explicit zero or false, so a default of true or nonzero can still be turned
// off in the config file.
func intFromPtr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func boolFromPtr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
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
