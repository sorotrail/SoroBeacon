package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/notify"
	"github.com/sorotrail/sorobeacon/internal/rules"
)

// openapiSchema is the slice of the spec the coverage test needs.
type openapiSchema struct {
	Paths map[string]map[string]any `json:"paths"`
}

// collectRoutes walks a chi router and returns every registered
// method/path pair.
func collectRoutes(t *testing.T, r chi.Router) []route {
	var out []route
	walk := func(method string, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi walks subroute roots with a trailing slash ("/monitors/");
		// the spec uses clean paths. Normalize so the two comparable sets
		// agree.
		if len(path) > 1 {
			path = strings.TrimRight(path, "/")
		}
		out = append(out, route{Method: method, Path: path})
		return nil
	}
	if err := chi.Walk(r, walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return out
}

type route struct {
	Method string
	Path   string
}

// TestOpenAPISpecCoversAllRoutes fails when a route exists in the router
// but not in the spec, or vice versa. This is the drift check: adding a
// handler without documenting it, or documenting an endpoint that does not
// exist, both fail the build.
func TestOpenAPISpecCoversAllRoutes(t *testing.T) {
	var doc openapiSchema
	if err := json.Unmarshal(openapiSpec, &doc); err != nil {
		t.Fatalf("openapi.json does not parse: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.json has no paths")
	}

	s := New(&fakeStore{}, rules.NewRegistry(), notify.DefaultFactory(), &fakeRPC{}, discardLogger())
	routes := collectRoutes(t, s.Routes())

	// Chi walks with the mounted pattern; the spec's server URL is /api/v1,
	// so route paths are compared directly.
	for _, rt := range routes {
		if rt.Path == "/*" || rt.Path == "/" && rt.Method != http.MethodGet {
			continue
		}
		ops, ok := doc.Paths[rt.Path]
		if !ok {
			t.Errorf("route %s %s is registered but missing from openapi.json", rt.Method, rt.Path)
			continue
		}
		methodKey := map[string]string{
			http.MethodGet:    "get",
			http.MethodPost:   "post",
			http.MethodPatch:  "patch",
			http.MethodPut:    "put",
			http.MethodDelete: "delete",
		}[rt.Method]
		if methodKey == "" {
			continue // HEAD/OPTIONS etc. are framework-added
		}
		if _, ok := ops[methodKey]; !ok {
			t.Errorf("route %s %s is registered but openapi.json has no %s entry for %q", rt.Method, rt.Path, methodKey, rt.Path)
		}
	}

	for path, ops := range doc.Paths {
		if path == "/metrics" {
			continue // served at the root tree, not under /api/v1
		}
		for method := range ops {
			// Path-level keys that are not operations.
			if method == "parameters" || method == "summary" || method == "description" || method == "servers" {
				continue
			}
			found := false
			for _, rt := range routes {
				if rt.Path == path {
					m := map[string]string{
						"get": http.MethodGet, "post": http.MethodPost,
						"patch": http.MethodPatch, "put": http.MethodPut,
						"delete": http.MethodDelete,
					}[method]
					if m == rt.Method {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("openapi.json documents %s %q but no such route is registered", method, path)
			}
		}
	}
}
