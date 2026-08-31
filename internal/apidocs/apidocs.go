// Package apidocs serves the embedded OpenAPI contract and interactive API
// explorer. Embedding keeps production documentation independent of the
// working directory and external CDNs.
package apidocs

import (
	"embed"
	"net/http"
)

//go:embed openapi.json docs.html
var assets embed.FS

func OpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
	w.Header().Set("Cache-Control", "no-cache")
	data, _ := assets.ReadFile("openapi.json")
	_, _ = w.Write(data)
}

func UI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, _ := assets.ReadFile("docs.html")
	_, _ = w.Write(data)
}

func Spec() []byte {
	data, _ := assets.ReadFile("openapi.json")
	return data
}
