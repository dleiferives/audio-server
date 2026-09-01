// Command test-live-stt exercises the real live WebSocket endpoint with a
// PCM16 mono 16 kHz WAV file and, optionally, waits for a refinement job.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	server := flag.String("server", "http://127.0.0.1:8010", "audio-server base URL")
	model := flag.String("model", "voxtral-realtime", "live streaming model")
	postModel := flag.String("post-process-model", "", "optional buffered refinement model")
	audioFile := flag.String("audio", "transcribe.cpp/samples/jfk.wav", "PCM16 mono 16 kHz WAV input")
	flag.Parse()

	pcm, err := readPCM16WAV(*audioFile)
	must(err)

	base, err := url.Parse(*server)
	must(err)
	if base.Scheme == "https" {
		base.Scheme = "wss"
	} else {
		base.Scheme = "ws"
	}
	base.Path = "/v1/audio/transcriptions/stream"
	query := base.Query()
	query.Set("model", *model)
	if *postModel != "" {
		query.Set("post_process_model", *postModel)
	}
	base.RawQuery = query.Encode()

	conn, response, err := websocket.DefaultDialer.Dial(base.String(), nil)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			fatalf("WebSocket handshake: %v (HTTP %d: %s)", err, response.StatusCode, strings.TrimSpace(string(body)))
		}
		fatalf("WebSocket handshake: %v", err)
	}
	defer conn.Close()

	events := make(chan map[string]any, 32)
	errs := make(chan error, 1)
	go func() {
		for {
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				errs <- err
				return
			}
			encoded, _ := json.Marshal(event)
			fmt.Println(string(encoded))
			events <- event
		}
	}()

	// Half-second frames are large enough to avoid excessive inference calls
	// while still exercising incremental updates.
	const frameBytes = 16000
	for offset := 0; offset < len(pcm); offset += frameBytes {
		end := min(offset+frameBytes, len(pcm))
		must(conn.WriteMessage(websocket.BinaryMessage, pcm[offset:end]))
	}
	must(conn.WriteJSON(map[string]string{"type": "input_audio.commit"}))

	var jobID string
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event["type"] == "transcription.post_process.created" {
				job, _ := event["job"].(map[string]any)
				jobID, _ = job["id"].(string)
			}
			if event["type"] == "transcription_session.done" {
				if jobID != "" {
					waitForJob(*server, jobID)
				}
				return
			}
		case err := <-errs:
			fatalf("reading WebSocket event: %v", err)
		case <-deadline.C:
			fatalf("timed out waiting for live transcription")
		}
	}
}

func waitForJob(server, jobID string) {
	jobURL := strings.TrimRight(server, "/") + "/v1/audio/transcription-jobs/" + url.PathEscape(jobID)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		response, err := http.Get(jobURL)
		must(err)
		var job map[string]any
		err = json.NewDecoder(response.Body).Decode(&job)
		response.Body.Close()
		must(err)
		encoded, _ := json.Marshal(job)
		fmt.Println(string(encoded))
		if job["status"] == "succeeded" {
			return
		}
		if job["status"] == "failed" {
			fatalf("post-processing failed: %v", job["error"])
		}
		time.Sleep(250 * time.Millisecond)
	}
	fatalf("timed out waiting for post-processing job %s", jobID)
}

func readPCM16WAV(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s is not a RIFF/WAVE file", path)
	}
	for offset := 12; offset+8 <= len(data); {
		name := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + size
		if end > len(data) {
			break
		}
		if name == "data" {
			return data[start:end], nil
		}
		offset = end + size%2
	}
	return nil, fmt.Errorf("%s has no valid data chunk", path)
}

func must(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "test-live-stt: "+format+"\n", args...)
	os.Exit(1)
}
