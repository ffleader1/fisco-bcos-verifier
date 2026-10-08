package api

import (
	"embed"
	"io/fs"
	"net/http"
)

// docsFS holds the API reference page: openapi.json (OpenAPI 3.0 description of every route) and a vendored
// copy of Swagger UI (swagger-ui-dist 5.33.1, Apache-2.0), so /docs/ works on servers without internet access.
//
//go:embed docs/index.html docs/openapi.json docs/swagger-ui
var docsFS embed.FS

// OpenAPISpec returns the embedded OpenAPI document.
func OpenAPISpec() []byte {
	b, _ := docsFS.ReadFile("docs/openapi.json")
	return b
}

// docsHandler serves /docs/ (Swagger UI) and /docs/openapi.json.
func docsHandler() http.Handler {
	sub, _ := fs.Sub(docsFS, "docs")
	flat := http.FileServer(http.FS(sub))
	ui, _ := fs.Sub(docsFS, "docs/swagger-ui")
	assets := http.FileServer(http.FS(ui))
	return http.StripPrefix("/docs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html", "/openapi.json":
			if r.URL.Path == "/openapi.json" {
				w.Header().Set("Cache-Control", "no-cache")
			}
			flat.ServeHTTP(w, r)
		default:
			w.Header().Set("Cache-Control", "public, max-age=86400")
			assets.ServeHTTP(w, r)
		}
	}))
}

// isDocsPath reports whether the path belongs to the public documentation (no API token needed).
func isDocsPath(p string) bool {
	return p == "/docs" || p == "/openapi.json" || len(p) >= 6 && p[:6] == "/docs/"
}
