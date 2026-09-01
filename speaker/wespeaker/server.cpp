#include "frontend/wav.h"
#include "speaker/speaker_engine.h"

#include <arpa/inet.h>
#include <glog/logging.h>
#include <netdb.h>
#include <sys/socket.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <limits>
#include <memory>
#include <sstream>
#include <string>
#include <unordered_map>
#include <vector>

namespace {

constexpr size_t kMaxRequestBytes = 64ULL * 1024 * 1024;

struct Options {
    std::string model;
    std::string host = "127.0.0.1";
    int port = 8034;
    int embedding_size = 256;
    int samples_per_chunk = 80000;
};

struct Request {
    std::string method;
    std::string path;
    std::unordered_map<std::string, std::string> headers;
    std::vector<char> body;
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

std::string json_escape(const std::string & value) {
    std::string out;
    for (const unsigned char c : value) {
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
    const char * reason = status == 200 ? "OK" : status == 400 ? "Bad Request" :
        status == 404 ? "Not Found" : "Internal Server Error";
    const std::string headers = "HTTP/1.1 " + std::to_string(status) + " " + reason +
        "\r\nContent-Type: application/json\r\nContent-Length: " + std::to_string(body.size()) +
        "\r\nConnection: close\r\n\r\n";
    write_all(fd, headers.data(), headers.size());
    write_all(fd, body.data(), body.size());
}

bool parse_options(int argc, char ** argv, Options & options) {
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto next = [&]() -> const char * { return i + 1 < argc ? argv[++i] : nullptr; };
        const char * value = nullptr;
        if (arg == "--model") {
            if ((value = next()) == nullptr) return false;
            options.model = value;
        } else if (arg == "--host") {
            if ((value = next()) == nullptr) return false;
            options.host = value;
        } else if (arg == "--port") {
            if ((value = next()) == nullptr) return false;
            options.port = std::atoi(value);
        } else if (arg == "--embedding-size") {
            if ((value = next()) == nullptr) return false;
            options.embedding_size = std::atoi(value);
        } else if (arg == "--samples-per-chunk") {
            if ((value = next()) == nullptr) return false;
            options.samples_per_chunk = std::atoi(value);
        } else {
            return false;
        }
    }
    return !options.model.empty() && options.port > 0 && options.port <= 65535 &&
        options.embedding_size > 0;
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
        if (colon != std::string::npos) {
            request.headers[lower(trim(line.substr(0, colon)))] = trim(line.substr(colon + 1));
        }
        if (end == std::string::npos) break;
        pos = end + 2;
    }
    size_t content_length = 0;
    if (const auto it = request.headers.find("content-length"); it != request.headers.end()) {
        try {
            const unsigned long long parsed = std::stoull(it->second);
            if (parsed > kMaxRequestBytes || parsed > std::numeric_limits<size_t>::max()) {
                throw std::out_of_range("size");
            }
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
    char pattern[] = "/tmp/wespeaker-XXXXXX.wav";
    const int fd = mkstemps(pattern, 4);
    if (fd < 0) { error = std::strerror(errno); return false; }
    path = pattern;
    size_t offset = 0;
    while (offset < body.size()) {
        const ssize_t n = write(fd, body.data() + offset, body.size() - offset);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) {
            error = std::strerror(errno);
            close(fd);
            unlink(path.c_str());
            return false;
        }
        offset += static_cast<size_t>(n);
    }
    close(fd);
    return true;
}

std::string embedding_json(const std::vector<float> & embedding, int samples) {
    std::ostringstream out;
    out << std::setprecision(9) << "{\"model\":\"wespeaker-gemini-dfresnet114-lm\",\"dimensions\":"
        << embedding.size() << ",\"duration\":" << (static_cast<double>(samples) / 16000.0)
        << ",\"embedding\":[";
    for (size_t i = 0; i < embedding.size(); ++i) {
        if (i != 0) out << ',';
        out << embedding[i];
    }
    out << "]}";
    return out.str();
}

}  // namespace

int main(int argc, char ** argv) {
    Options options;
    if (!parse_options(argc, argv, options)) {
        std::cerr << "usage: wespeaker_server --model FILE [--host HOST] [--port PORT] "
                     "[--embedding-size N] [--samples-per-chunk N]\n";
        return 2;
    }
    google::InitGoogleLogging(argv[0]);
    std::unique_ptr<wespeaker::SpeakerEngine> engine;
    try {
        engine = std::make_unique<wespeaker::SpeakerEngine>(
            options.model, 80, 16000, options.embedding_size, options.samples_per_chunk);
    } catch (const std::exception & ex) {
        std::cerr << "WeSpeaker model load failed: " << ex.what() << '\n';
        return 1;
    }
    const int listener = listen_socket(options);
    if (listener < 0) {
        std::cerr << "cannot listen on " << options.host << ':' << options.port << ": " << std::strerror(errno) << '\n';
        return 1;
    }
    std::cout << "WeSpeaker ONNX server listening on " << options.host << ':' << options.port << '\n';
    for (;;) {
        const int client = accept(listener, nullptr, nullptr);
        if (client < 0 && errno == EINTR) continue;
        if (client < 0) break;
        Request request;
        std::string error;
        if (!read_request(client, request, error)) {
            respond(client, 400, "{\"error\":{\"message\":\"" + json_escape(error) + "\"}}");
            close(client);
            continue;
        }
        if (request.method == "GET" && request.path == "/health") {
            respond(client, 200, "{\"status\":\"ok\",\"model\":\"wespeaker-gemini-dfresnet114-lm\"}");
            close(client);
            continue;
        }
        if (request.method != "POST" || request.path != "/embed") {
            respond(client, 404, "{\"error\":{\"message\":\"not found\"}}");
            close(client);
            continue;
        }
        if (request.body.empty()) {
            respond(client, 400, "{\"error\":{\"message\":\"WAV audio body is required\"}}");
            close(client);
            continue;
        }
        std::string wav_path;
        if (!write_temp_wav(request.body, wav_path, error)) {
            respond(client, 500, "{\"error\":{\"message\":\"temporary WAV: " + json_escape(error) + "\"}}");
            close(client);
            continue;
        }
        try {
            wenet::WavReader wav(wav_path);
            unlink(wav_path.c_str());
            if (wav.sample_rate() != 16000) {
                respond(client, 400, "{\"error\":{\"message\":\"audio must be mono 16 kHz PCM16 WAV\"}}");
            } else if (wav.num_sample() < 4000) {
                respond(client, 400, "{\"error\":{\"message\":\"at least 250 ms of speech is required\"}}");
            } else {
                std::vector<float> embedding(options.embedding_size, 0.0f);
                engine->ExtractEmbedding(wav.data(), wav.num_sample(), &embedding);
                respond(client, 200, embedding_json(embedding, wav.num_sample()));
            }
        } catch (const std::exception & ex) {
            unlink(wav_path.c_str());
            respond(client, 400, "{\"error\":{\"message\":\"invalid WAV: " + json_escape(ex.what()) + "\"}}");
        }
        close(client);
    }
    close(listener);
    return 0;
}
