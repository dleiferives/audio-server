package apidocs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOpenAPIContract(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got := doc["openapi"]; got != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", got)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("paths is missing or invalid")
	}
	wantPaths := []string{
		"/docs", "/openapi.json", "/healthz", "/v1/audio/capabilities",
		"/v1/audio/voices", "/v1/audio/speech", "/v1/audio/jobs",
		"/v1/audio/jobs/{id}", "/v1/audio/jobs/{id}/audio",
		"/v1/audio/jobs/{id}/stream", "/v1/audio/transcriptions",
		"/v1/audio/transcriptions/stream", "/v1/audio/transcription-jobs/{id}",
		"/v1/audio/analysis-jobs", "/v1/audio/analysis-jobs/{id}",
		"/v1/audio/analysis-jobs/{id}/result",
		"/v1/audio/alignments", "/v1/audio/alignments/models",
		"/v1/audio/alignments/{id}", "/v1/audio/alignments/{id}/result",
	}
	for _, path := range wantPaths {
		if _, ok := paths[path]; !ok {
			t.Errorf("missing path %s", path)
		}
	}

	operationIDs := map[string]bool{}
	for path, rawItem := range paths {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Errorf("path %s is not an object", path)
			continue
		}
		for method, rawOperation := range item {
			if !isHTTPMethod(method) {
				continue
			}
			operation, ok := rawOperation.(map[string]any)
			if !ok {
				t.Errorf("%s %s is not an object", method, path)
				continue
			}
			id, _ := operation["operationId"].(string)
			if id == "" {
				t.Errorf("%s %s has no operationId", method, path)
			} else if operationIDs[id] {
				t.Errorf("duplicate operationId %q", id)
			}
			operationIDs[id] = true
			if _, ok := operation["responses"].(map[string]any); !ok {
				t.Errorf("%s %s has no responses", method, path)
			}
			if _, ok := operation["tags"].([]any); !ok {
				t.Errorf("%s %s has no tags", method, path)
			}
		}
	}
	validateRefs(t, doc, doc, "$")
}

func TestDocsUIIsSelfContained(t *testing.T) {
	data, err := assets.ReadFile("docs.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, external := range []string{"<script src=", "<link rel=\"stylesheet\" href=\"http"} {
		if strings.Contains(html, external) {
			t.Fatalf("docs UI contains external asset %q", external)
		}
	}
	for _, feature := range []string{"/openapi.json", "Send request", "Copy curl", "Bearer token"} {
		if !strings.Contains(html, feature) {
			t.Errorf("docs UI is missing %q", feature)
		}
	}
}

func validateRefs(t *testing.T, root, value any, path string) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#/") {
					t.Errorf("%s has invalid local ref %v", path, child)
					continue
				}
				if _, ok := resolveJSONPointer(root, ref); !ok {
					t.Errorf("%s has unresolved ref %s", path, ref)
				}
			}
			validateRefs(t, root, child, path+"/"+key)
		}
	case []any:
		for i, child := range value {
			validateRefs(t, root, child, fmt.Sprintf("%s/%d", path, i))
		}
	}
}

func resolveJSONPointer(root any, ref string) (any, bool) {
	current := root
	for _, token := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[token]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func isHTTPMethod(method string) bool {
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options", "trace":
		return true
	default:
		return false
	}
}
