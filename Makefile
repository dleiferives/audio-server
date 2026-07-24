# ── provider toggles ──
ESPEAK        ?= 1
OMNIVOICE     ?= 1
KOKORO        ?= 0
FASTERWHISPER ?= 0
ALIGN         ?= 0

# ── ports ──
SERVER_PORT    ?= 8010
OMNIVOICE_PORT ?= 8020
SUPERTONIC_PORT ?= 8022
KOKORO_PORT    ?= 8021
NEMOTRON_PORT  ?= 8024
WHISPER_PORT   ?= 8030

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

.PHONY: build run stop clean

build:
	@echo "  → building $(BIN)"
	@mkdir -p bin
	go build -o $(BIN) ./cmd/audio
	@echo "  → built $(BIN)"

$(AUDIOCPP_SENTINEL):
	@echo "  → building audiocpp_server (one-time, ~5-10 min)..."
	cd audio.cpp && bash scripts/build_linux.sh --backend cuda --target audiocpp_server
	@touch $(AUDIOCPP_SENTINEL)
	@echo "  → audiocpp_server built"

run: build
	@if [ "$(OMNIVOICE)" = "1" ] && [ ! -f "$(AUDIOCPP_SENTINEL)" ]; then \
		$(MAKE) $(AUDIOCPP_SENTINEL); \
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
	if [ "$(FASTERWHISPER)" = "1" ]; then \
		echo '  →   launching faster-whisper sidecar on :$(WHISPER_PORT)'; \
		./stt/fasterwhisper/server.py --port $(WHISPER_PORT) & echo $$! > $(PIDIR)/whisper.pid; \
	fi; \
	if [ "$(OMNIVOICE)" = "1" ]; then \
		echo '  →   skipping wait for lifecycle-managed GPU models'; \
	fi; \
	if [ "$(KOKORO)" = "1" ]; then \
		echo '  →   waiting for kokoro...'; \
		for i in $$(seq 1 20); do \
			curl -sS http://127.0.0.1:$(KOKORO_PORT)/health > /dev/null 2>&1 && break; \
			sleep 0.5; \
		done; \
	fi; \
	if [ "$(FASTERWHISPER)" = "1" ]; then \
		echo '  →   waiting for faster-whisper...'; \
		for i in $$(seq 1 20); do \
			curl -sS http://127.0.0.1:$(WHISPER_PORT)/health > /dev/null 2>&1 && break; \
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
	rm -rf $(BIN) $(PIDIR) $(AUDIOCPP_SENTINEL)
	@echo "  → cleaned (use make clean-all to also remove audio.cpp build dir)"

clean-all: clean
	rm -rf audio.cpp/build
	@echo "  → fully cleaned"
