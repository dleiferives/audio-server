# ── provider toggles ──
ESPEAK        ?= 1
OMNIVOICE     ?= 1
KOKORO        ?= 0
ALIGN         ?= 0
TRANSCRIBECPP ?= 1

# ── ports ──
SERVER_PORT    ?= 8010
OMNIVOICE_PORT ?= 8020
SUPERTONIC_PORT ?= 8022
KOKORO_PORT    ?= 8021
NEMOTRON_PORT  ?= 8024

# ── frontend ──
WEB_DIR ?= $(CURDIR)/web

# ── server settings ──
DEFAULT_PROVIDER ?= espeak-ng
MAX_CONCURRENCY  ?= 2
TIMEOUT          ?= 0
MAX_CHARS        ?= 1000000
ESPEAK_VOICE     ?= en
MP3_BITRATE      ?= 48k

BIN        := bin/audio-server
PIDIR      := .pids
AUDIOCPP_BIN := audio.cpp/build/linux-cuda-release/bin/audiocpp_server
AUDIOCPP_CFG := audio.cpp/omnivoice-server.json
AUDIOCPP_SENTINEL := .built-audiocpp
TRANSCRIBECPP_BUILD := transcribe.cpp/build/linux-cuda-release
TRANSCRIBECPP_BIN := bin/transcribecpp_server
TRANSCRIBECPP_SENTINEL := .built-transcribecpp
COHERE_MODEL := models/cohere-transcribe-03-2026-Q8_0.gguf
COHERE_MODEL_URL := https://huggingface.co/handy-computer/cohere-transcribe-03-2026-gguf/resolve/main/cohere-transcribe-03-2026-Q8_0.gguf
VOXTRAL_MODEL := models/Voxtral-Mini-4B-Realtime-2602-Q4_K_M.gguf
VOXTRAL_MODEL_URL := https://huggingface.co/handy-computer/Voxtral-Mini-4B-Realtime-2602-gguf/resolve/main/Voxtral-Mini-4B-Realtime-2602-Q4_K_M.gguf
CUDA_HOME ?= /usr/local/cuda

.PHONY: build build-audiocpp build-transcribecpp download-cohere download-voxtral run stop clean

build:
	@echo "  → building $(BIN)"
	@mkdir -p bin
	go build -o $(BIN) ./cmd/audio
	@echo "  → built $(BIN)"

build-audiocpp: $(AUDIOCPP_SENTINEL)

build-transcribecpp: $(TRANSCRIBECPP_SENTINEL)

download-cohere: $(COHERE_MODEL)

download-voxtral: $(VOXTRAL_MODEL)

$(AUDIOCPP_SENTINEL):
	@echo "  → building audiocpp_server (one-time, ~5-10 min)..."
	@test -x "$(CUDA_HOME)/bin/nvcc" || { \
		echo "CUDA compiler not found at $(CUDA_HOME)/bin/nvcc" >&2; \
		exit 1; \
	}
	cd audio.cpp && \
		PATH="$(CUDA_HOME)/bin:$$PATH" \
		CUDAToolkit_ROOT="$(CUDA_HOME)" \
		CUDACXX="$(CUDA_HOME)/bin/nvcc" \
		bash scripts/build_linux.sh \
			--backend cuda \
			--model-set full \
			--target audiocpp_server
	@touch $(AUDIOCPP_SENTINEL)
	@echo "  → audiocpp_server built"

$(TRANSCRIBECPP_SENTINEL): stt/transcribecpp/server.cpp
	@echo "  → building transcribe.cpp CUDA runtime and sidecar..."
	@test -x "$(CUDA_HOME)/bin/nvcc" || { \
		echo "CUDA compiler not found at $(CUDA_HOME)/bin/nvcc" >&2; \
		exit 1; \
	}
	cmake -S transcribe.cpp -B $(TRANSCRIBECPP_BUILD) -G Ninja \
		-DCMAKE_BUILD_TYPE=Release \
		-DTRANSCRIBE_CUDA=ON \
		-DTRANSCRIBE_BUILD_TESTS=ON \
		-DCMAKE_CUDA_COMPILER="$(CUDA_HOME)/bin/nvcc"
	cmake --build $(TRANSCRIBECPP_BUILD) --target transcribe-cli -j "$$(nproc)"
	@mkdir -p bin
	c++ -std=c++17 -O3 -DNDEBUG -DTRANSCRIBE_STATIC \
		-Itranscribe.cpp/include -Itranscribe.cpp/examples/common \
		stt/transcribecpp/server.cpp -o $(TRANSCRIBECPP_BIN) \
		$(TRANSCRIBECPP_BUILD)/src/libtranscribe.a \
		$(TRANSCRIBECPP_BUILD)/examples/common/libtranscribe-common-example.a \
		$(TRANSCRIBECPP_BUILD)/ggml/src/libggml.a \
		$(TRANSCRIBECPP_BUILD)/ggml/src/libggml-cpu.a \
		$(TRANSCRIBECPP_BUILD)/ggml/src/ggml-cuda/libggml-cuda.a \
		$(TRANSCRIBECPP_BUILD)/ggml/src/libggml-base.a \
		-L$(CUDA_HOME)/lib64 -Wl,-rpath,$(CUDA_HOME)/lib64 \
		-lm -lcudart -lcublas -lcublasLt -lculibos -lcuda -ldl -lrt -pthread
	@touch $(TRANSCRIBECPP_SENTINEL)
	@echo "  → transcribecpp_server built"

$(COHERE_MODEL):
	@mkdir -p models
	@echo "  → downloading Cohere Transcribe Q8 model (~2.4 GB)..."
	curl --fail --location --continue-at - --output "$@" "$(COHERE_MODEL_URL)"
	@echo "  → downloaded $@"

$(VOXTRAL_MODEL):
	@mkdir -p models
	@echo "  → downloading Voxtral Realtime Q4 model (~2.8 GB)..."
	curl --fail --location --continue-at - --output "$@" "$(VOXTRAL_MODEL_URL)"
	@echo "  → downloaded $@"

run: build
	@if [ "$(OMNIVOICE)" = "1" ] && [ ! -f "$(AUDIOCPP_SENTINEL)" ]; then \
		$(MAKE) $(AUDIOCPP_SENTINEL); \
	fi
	@if [ "$(TRANSCRIBECPP)" = "1" ] && [ ! -f "$(TRANSCRIBECPP_SENTINEL)" ]; then \
		$(MAKE) $(TRANSCRIBECPP_SENTINEL); \
	fi
	@if [ "$(TRANSCRIBECPP)" = "1" ] && [ ! -f "$(COHERE_MODEL)" ]; then \
		$(MAKE) $(COHERE_MODEL); \
	fi
	@if [ "$(TRANSCRIBECPP)" = "1" ] && [ ! -f "$(VOXTRAL_MODEL)" ]; then \
		$(MAKE) $(VOXTRAL_MODEL); \
	fi
	@mkdir -p $(PIDIR)
	@fuser -k $(OMNIVOICE_PORT)/tcp 2>/dev/null && sleep 0.5 || true
	@fuser -k $(SERVER_PORT)/tcp 2>/dev/null && sleep 0.5 || true
	@trap '$(MAKE) --no-print-directory stop' INT TERM; \
	set -e; \
	echo '  → starting providers...'; \
	if [ "$(ESPEAK)" = "1" ]; then \
		echo '  →   espeak-ng ready'; \
	fi; \
	if [ "$(OMNIVOICE)" = "1" ]; then \
		echo '  →   GPU models managed by lifecycle (start on first request, idle after $(AUDIO_AUDIOCPP_IDLE_UNLOAD)s)'; \
	fi; \
	if [ "$(KOKORO)" = "1" ]; then \
		echo '  →   launching kokoro sidecar on :$(KOKORO_PORT)'; \
		./tts/kokoro/server.py --port $(KOKORO_PORT) & echo $$! > $(PIDIR)/kokoro.pid; \
	fi; \
	if [ "$(OMNIVOICE)" = "1" ]; then \
		echo '  →   skipping wait for lifecycle-managed GPU models (including faster-whisper)'; \
	fi; \
	if [ "$(KOKORO)" = "1" ]; then \
		echo '  →   waiting for kokoro...'; \
		for i in $$(seq 1 20); do \
			curl -sS http://127.0.0.1:$(KOKORO_PORT)/health > /dev/null 2>&1 && break; \
			sleep 0.5; \
		done; \
	fi; \
	echo '  → starting audio server on :$(SERVER_PORT)'; \
	$(BIN) \
		-config=$(CURDIR)/config.yml & echo $$! > $(PIDIR)/server.pid; \
	wait $$(cat $(PIDIR)/server.pid) || true; \
	$(MAKE) --no-print-directory stop

stop:
	@for pidfile in $(PIDIR)/*.pid; do \
		[ -f "$$pidfile" ] || continue; \
		pid=$$(cat "$$pidfile" 2>/dev/null); \
		[ -n "$$pid" ] && kill $$pid 2>/dev/null || true; \
		rm -f "$$pidfile"; \
	done
	@echo "  → all stopped"

clean:
	rm -rf $(BIN) $(TRANSCRIBECPP_BIN) $(PIDIR) $(AUDIOCPP_SENTINEL) $(TRANSCRIBECPP_SENTINEL)
	@echo "  → cleaned (use make clean-all to also remove audio.cpp build dir)"

clean-all: clean
	rm -rf audio.cpp/build transcribe.cpp/build
	@echo "  → fully cleaned (downloaded models retained)"
