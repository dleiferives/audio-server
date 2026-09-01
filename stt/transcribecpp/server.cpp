#include "transcribe.h"
#include "wav.h"

#include <arpa/inet.h>
#include <netdb.h>
#include <sys/socket.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <limits>
#include <string>
#include <unordered_map>
#include <vector>

namespace {

constexpr size_t kMaxRequestBytes = 512ULL * 1024 * 1024;

struct Options {
    std::string model;
    std::string host = "127.0.0.1";
    int port = 8031;
    int threads = 0;
    transcribe_backend_request backend = TRANSCRIBE_BACKEND_AUTO;
};

std::string lower(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(), [](unsigned char c) { return std::tolower(c); });
    return value;
}

std::string trim(std::string value) {
    const auto first = value.find_first_not_of(" \t\r\n");
    if (first == std::string::npos) return {};
    const auto last = value.find_last_not_of(" \t\r\n");
    return value.substr(first, last - first + 1);
}

std::string json_escape(const char * value) {
    std::string out;
    if (!value) return out;
    for (const unsigned char c : std::string(value)) {
        switch (c) {
            case '\\': out += "\\\\"; break;
            case '"': out += "\\\""; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (c < 0x20) {
                    char buf[7];
                    std::snprintf(buf, sizeof(buf), "\\u%04x", c);
                    out += buf;
                } else {
                    out += static_cast<char>(c);
                }
        }
    }
    return out;
}

bool write_all(int fd, const char * data, size_t size) {
    while (size > 0) {
        const ssize_t n = ::send(fd, data, size, MSG_NOSIGNAL);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return false;
        data += n;
        size -= static_cast<size_t>(n);
    }
    return true;
}

void respond(int fd, int status, const std::string & body) {
    const char * reason = status == 200 ? "OK" : status == 400 ? "Bad Request" : status == 404 ? "Not Found" :
                                             status == 409       ? "Conflict" :
                                                                   "Internal Server Error";
    const std::string header = "HTTP/1.1 " + std::to_string(status) + " " + reason +
                               "\r\nContent-Type: application/json\r\nContent-Length: " +
                               std::to_string(body.size()) + "\r\nConnection: close\r\n\r\n";
    write_all(fd, header.data(), header.size());
    write_all(fd, body.data(), body.size());
}

bool parse_options(int argc, char ** argv, Options & options) {
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto next = [&]() -> const char * { return i + 1 < argc ? argv[++i] : nullptr; };
        if (arg == "--model") {
            const char * value = next();
            if (!value) return false;
            options.model = value;
        } else if (arg == "--host") {
            const char * value = next();
            if (!value) return false;
            options.host = value;
        } else if (arg == "--port") {
            const char * value = next();
            if (!value) return false;
            options.port = std::atoi(value);
        } else if (arg == "--threads") {
            const char * value = next();
            if (!value) return false;
            options.threads = std::atoi(value);
        } else if (arg == "--backend") {
            const char * value = next();
            if (!value) return false;
            const std::string backend = lower(value);
            if (backend == "auto") options.backend = TRANSCRIBE_BACKEND_AUTO;
            else if (backend == "cuda") options.backend = TRANSCRIBE_BACKEND_CUDA;
            else if (backend == "cpu") options.backend = TRANSCRIBE_BACKEND_CPU;
            else return false;
        } else {
            return false;
        }
    }
    return !options.model.empty() && options.port > 0 && options.port <= 65535 && options.threads >= 0;
}

int listen_socket(const Options & options) {
    addrinfo hints{};
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    hints.ai_flags = AI_PASSIVE;
    addrinfo * addresses = nullptr;
    const std::string port = std::to_string(options.port);
    if (getaddrinfo(options.host.c_str(), port.c_str(), &hints, &addresses) != 0) return -1;

    int listener = -1;
    for (addrinfo * address = addresses; address; address = address->ai_next) {
        listener = socket(address->ai_family, address->ai_socktype, address->ai_protocol);
        if (listener < 0) continue;
        int one = 1;
        setsockopt(listener, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
        if (bind(listener, address->ai_addr, address->ai_addrlen) == 0 && listen(listener, 16) == 0) break;
        close(listener);
        listener = -1;
    }
    freeaddrinfo(addresses);
    return listener;
}

struct Request {
    std::string method;
    std::string path;
    std::unordered_map<std::string, std::string> headers;
    std::vector<char> body;
};

bool read_request(int fd, Request & request, std::string & error) {
    std::vector<char> bytes;
    bytes.reserve(64 * 1024);
    size_t header_end = std::string::npos;
    char chunk[16 * 1024];
    while (header_end == std::string::npos) {
        const ssize_t n = recv(fd, chunk, sizeof(chunk), 0);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) { error = "incomplete request headers"; return false; }
        bytes.insert(bytes.end(), chunk, chunk + n);
        if (bytes.size() > 64 * 1024) { error = "request headers too large"; return false; }
        const std::string view(bytes.begin(), bytes.end());
        header_end = view.find("\r\n\r\n");
    }

    const std::string headers(bytes.data(), header_end);
    const auto first_line_end = headers.find("\r\n");
    const std::string first_line = headers.substr(0, first_line_end);
    const auto first_space = first_line.find(' ');
    const auto second_space = first_line.find(' ', first_space + 1);
    if (first_space == std::string::npos || second_space == std::string::npos) {
        error = "malformed request line";
        return false;
    }
    request.method = first_line.substr(0, first_space);
    request.path = first_line.substr(first_space + 1, second_space - first_space - 1);

    size_t pos = first_line_end == std::string::npos ? headers.size() : first_line_end + 2;
    while (pos < headers.size()) {
        const auto end = headers.find("\r\n", pos);
        const std::string line = headers.substr(pos, end == std::string::npos ? std::string::npos : end - pos);
        const auto colon = line.find(':');
        if (colon != std::string::npos) request.headers[lower(trim(line.substr(0, colon)))] = trim(line.substr(colon + 1));
        if (end == std::string::npos) break;
        pos = end + 2;
    }

    size_t content_length = 0;
    if (const auto it = request.headers.find("content-length"); it != request.headers.end()) {
        try {
            const unsigned long long parsed = std::stoull(it->second);
            if (parsed > kMaxRequestBytes || parsed > std::numeric_limits<size_t>::max()) throw std::out_of_range("size");
            content_length = static_cast<size_t>(parsed);
        } catch (...) {
            error = "invalid or excessive Content-Length";
            return false;
        }
    }
    const size_t body_start = header_end + 4;
    request.body.insert(request.body.end(), bytes.begin() + static_cast<std::ptrdiff_t>(body_start), bytes.end());
    while (request.body.size() < content_length) {
        const size_t wanted = std::min(sizeof(chunk), content_length - request.body.size());
        const ssize_t n = recv(fd, chunk, wanted, 0);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) { error = "incomplete request body"; return false; }
        request.body.insert(request.body.end(), chunk, chunk + n);
    }
    if (request.body.size() > content_length) request.body.resize(content_length);
    return true;
}

bool write_temp_wav(const std::vector<char> & body, std::string & path, std::string & error) {
    char pattern[] = "/tmp/transcribecpp-XXXXXX.wav";
    const int fd = mkstemps(pattern, 4);
    if (fd < 0) { error = std::strerror(errno); return false; }
    path = pattern;
    size_t offset = 0;
    while (offset < body.size()) {
        const ssize_t n = write(fd, body.data() + offset, body.size() - offset);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) { error = std::strerror(errno); close(fd); unlink(path.c_str()); return false; }
        offset += static_cast<size_t>(n);
    }
    close(fd);
    return true;
}

std::string stream_snapshot(transcribe_session * session, const transcribe_stream_update & update) {
    transcribe_stream_text text;
    transcribe_stream_text_init(&text);
    const transcribe_status status = transcribe_stream_get_text(session, &text);
    if (status != TRANSCRIBE_OK) {
        return "{\"error\":{\"message\":\"stream snapshot failed: " +
               json_escape(transcribe_status_string(status)) + "\"}}";
    }
    return "{\"text\":\"" + json_escape(text.full_text) + "\",\"committed_text\":\"" +
           json_escape(text.committed_text) + "\",\"tentative_text\":\"" + json_escape(text.tentative_text) +
           "\",\"revision\":" + std::to_string(update.revision) +
           ",\"input_ms\":" + std::to_string(update.input_received_ms) +
           ",\"buffered_ms\":" + std::to_string(update.buffered_ms) +
           ",\"changed\":" + (update.result_changed ? "true" : "false") +
           ",\"final\":" + (update.is_final ? "true" : "false") + "}";
}

bool pcm16_to_float(const std::vector<char> & bytes, std::vector<float> & pcm, std::string & error) {
    if (bytes.empty() || bytes.size() % 2 != 0) {
        error = "PCM body must contain complete signed 16-bit little-endian samples";
        return false;
    }
    pcm.resize(bytes.size() / 2);
    for (size_t i = 0; i < pcm.size(); ++i) {
        const uint16_t lo = static_cast<unsigned char>(bytes[i * 2]);
        const uint16_t hi = static_cast<unsigned char>(bytes[i * 2 + 1]);
        const int16_t sample = static_cast<int16_t>(lo | (hi << 8));
        pcm[i] = static_cast<float>(sample) / 32768.0f;
    }
    return true;
}

}  // namespace

int main(int argc, char ** argv) {
    Options options;
    if (!parse_options(argc, argv, options)) {
        std::cerr << "usage: transcribecpp_server --model FILE [--host HOST] [--port PORT] "
                     "[--backend auto|cuda|cpu] [--threads N]\n";
        return 2;
    }

    transcribe_model_load_params load_params;
    transcribe_model_load_params_init(&load_params);
    load_params.backend = options.backend;
    transcribe_model * model = nullptr;
    const transcribe_status load_status = transcribe_model_load_file(options.model.c_str(), &load_params, &model);
    if (load_status != TRANSCRIBE_OK) {
        std::cerr << "model load failed: " << transcribe_status_string(load_status) << '\n';
        return 1;
    }

    transcribe_session_params session_params;
    transcribe_session_params_init(&session_params);
    session_params.n_threads = options.threads;
    transcribe_session * session = nullptr;
    const transcribe_status session_status = transcribe_session_init(model, &session_params, &session);
    if (session_status != TRANSCRIBE_OK) {
        std::cerr << "session init failed: " << transcribe_status_string(session_status) << '\n';
        transcribe_model_free(model);
        return 1;
    }

    const int listener = listen_socket(options);
    if (listener < 0) {
        std::cerr << "cannot listen on " << options.host << ':' << options.port << ": " << std::strerror(errno) << '\n';
        transcribe_session_free(session);
        transcribe_model_free(model);
        return 1;
    }
    std::cout << "transcribe.cpp " << transcribe_version() << " (" << transcribe_model_backend(model) << ") listening on "
              << options.host << ':' << options.port << '\n';

    for (;;) {
        const int client = accept(listener, nullptr, nullptr);
        if (client < 0 && errno == EINTR) continue;
        if (client < 0) break;
        Request request;
        std::string error;
        if (!read_request(client, request, error)) {
            respond(client, 400, "{\"error\":{\"message\":\"" + json_escape(error.c_str()) + "\"}}");
            close(client);
            continue;
        }
        if (request.method == "GET" && request.path == "/health") {
            respond(client, 200, "{\"status\":\"ok\"}");
            close(client);
            continue;
        }
        if (request.method != "POST" || request.path != "/transcribe") {
            if (request.method == "POST" && request.path == "/stream/begin") {
                if (transcribe_stream_get_state(session) == TRANSCRIBE_STREAM_ACTIVE) {
                    respond(client, 409, "{\"error\":{\"message\":\"a stream is already active\"}}");
                } else {
                    transcribe_run_params run_params;
                    transcribe_run_params_init(&run_params);
                    std::string language;
                    if (const auto it = request.headers.find("x-transcribe-language"); it != request.headers.end()) {
                        language = trim(it->second);
                        if (!language.empty() && language != "auto") run_params.language = language.c_str();
                    }
                    transcribe_stream_params stream_params;
                    transcribe_stream_params_init(&stream_params);
                    const transcribe_status status = transcribe_stream_begin(session, &run_params, &stream_params);
                    if (status == TRANSCRIBE_OK) {
                        respond(client, 200, "{\"status\":\"ready\"}");
                    } else {
                        respond(client, 400, "{\"error\":{\"message\":\"stream begin failed: " +
                                                 json_escape(transcribe_status_string(status)) + "\"}}");
                    }
                }
                close(client);
                continue;
            }
            if (request.method == "POST" && request.path == "/stream/feed") {
                std::vector<float> pcm;
                if (!pcm16_to_float(request.body, pcm, error)) {
                    respond(client, 400, "{\"error\":{\"message\":\"" + json_escape(error.c_str()) + "\"}}");
                } else {
                    transcribe_stream_update update;
                    transcribe_stream_update_init(&update);
                    const transcribe_status status =
                        transcribe_stream_feed(session, pcm.data(), static_cast<int>(pcm.size()), &update);
                    if (status == TRANSCRIBE_OK) {
                        respond(client, 200, stream_snapshot(session, update));
                    } else {
                        respond(client, 400, "{\"error\":{\"message\":\"stream feed failed: " +
                                                 json_escape(transcribe_status_string(status)) + "\"}}");
                    }
                }
                close(client);
                continue;
            }
            if (request.method == "POST" && request.path == "/stream/finalize") {
                transcribe_stream_update update;
                transcribe_stream_update_init(&update);
                const transcribe_status status = transcribe_stream_finalize(session, &update);
                if (status == TRANSCRIBE_OK) {
                    respond(client, 200, stream_snapshot(session, update));
                } else {
                    respond(client, 400, "{\"error\":{\"message\":\"stream finalize failed: " +
                                             json_escape(transcribe_status_string(status)) + "\"}}");
                }
                close(client);
                continue;
            }
            if (request.method == "POST" && request.path == "/stream/reset") {
                transcribe_stream_reset(session);
                respond(client, 200, "{\"status\":\"reset\"}");
                close(client);
                continue;
            }
            respond(client, 404, "{\"error\":{\"message\":\"not found\"}}");
            close(client);
            continue;
        }
        if (transcribe_stream_get_state(session) == TRANSCRIBE_STREAM_ACTIVE) {
            respond(client, 409, "{\"error\":{\"message\":\"cannot run offline transcription during an active stream\"}}");
            close(client);
            continue;
        }
        if (request.body.empty()) {
            respond(client, 400, "{\"error\":{\"message\":\"audio body is required\"}}");
            close(client);
            continue;
        }

        std::string wav_path;
        if (!write_temp_wav(request.body, wav_path, error)) {
            respond(client, 500, "{\"error\":{\"message\":\"temporary WAV: " + json_escape(error.c_str()) + "\"}}");
            close(client);
            continue;
        }
        std::vector<float> pcm;
        const bool loaded = transcribe_cli::load_wav_mono_16k(wav_path, pcm, error);
        unlink(wav_path.c_str());
        if (!loaded) {
            respond(client, 400, "{\"error\":{\"message\":\"" + json_escape(error.c_str()) + "\"}}");
            close(client);
            continue;
        }

        transcribe_run_params run_params;
        transcribe_run_params_init(&run_params);
        std::string language;
        if (const auto it = request.headers.find("x-transcribe-language"); it != request.headers.end()) {
            language = trim(it->second);
            if (!language.empty()) run_params.language = language.c_str();
        }
        const transcribe_status run_status = transcribe_run(session, pcm.data(), static_cast<int>(pcm.size()), &run_params);
        if (run_status != TRANSCRIBE_OK && run_status != TRANSCRIBE_ERR_OUTPUT_TRUNCATED) {
            respond(client, 500, "{\"error\":{\"message\":\"transcription failed: " +
                                     json_escape(transcribe_status_string(run_status)) + "\"}}");
            close(client);
            continue;
        }
        const char * text = transcribe_full_text(session);
        const char * detected = transcribe_detected_language(session);
        const double duration = static_cast<double>(pcm.size()) / 16000.0;
        const std::string body = "{\"text\":\"" + json_escape(text) + "\",\"language\":\"" +
                                 json_escape(detected) + "\",\"duration\":" + std::to_string(duration) + "}";
        respond(client, 200, body);
        close(client);
    }

    close(listener);
    transcribe_session_free(session);
    transcribe_model_free(model);
    return 0;
}
