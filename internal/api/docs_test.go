package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"sellergraph/fisco/internal/api"
)

// Every route the server registers must be documented, and the spec must not document routes that don't exist.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(api.OpenAPISpec(), &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}
	served := map[string]bool{}
	for _, r := range (&api.Server{}).Routes() {
		served[r.Method+" "+r.Pattern] = true
	}
	var missing, extra []string
	for k := range served {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("spec out of sync with routes\nundocumented: %v\nno such route: %v", missing, extra)
	}
}

func TestDocsServedWithoutToken(t *testing.T) {
	ts, _ := newServer(t, "secret")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(ts.URL + "/docs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "docs/" {
		t.Fatalf("/docs should redirect to docs/, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for path, want := range map[string]string{
		"/docs/":                     "swagger-ui-bundle.js",
		"/docs/openapi.json":         `"openapi": "3.0.3"`,
		"/openapi.json":              `"openapi": "3.0.3"`,
		"/docs/swagger-ui-bundle.js": "SwaggerUIBundle",
		"/docs/swagger-ui.css":       ".swagger-ui",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: status %d, contains %q = %v", path, resp.StatusCode, want, strings.Contains(string(b), want))
		}
	}
	// the API itself still needs the token
	if resp, _ := http.Get(ts.URL + "/v1/loans"); resp.StatusCode != 401 {
		t.Fatalf("API must still require the token, got %d", resp.StatusCode)
	}
}
