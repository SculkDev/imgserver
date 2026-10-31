package main

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiSpec []byte

const docsHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>imgserver API</title><link rel="icon" href="data:,"><link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.32.0/swagger-ui.css"></head>
<body><div id="swagger-ui"></div><script src="https://unpkg.com/swagger-ui-dist@5.32.0/swagger-ui-bundle.js" crossorigin="anonymous"></script>
<script src="/docs/init.js"></script></body></html>`

const docsJS = `window.ui = SwaggerUIBundle({url: "/openapi.json", dom_id: "#swagger-ui", deepLinking: true, persistAuthorization: false, validatorUrl: null, supportedSubmitMethods: ["get", "head", "post", "delete"]});`

func docsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(openapiSpec)
	})
	for _, path := range []string{"/docs", "/docs/"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/docs" && r.URL.Path != "/docs/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' https://unpkg.com; style-src 'self' 'unsafe-inline' https://unpkg.com; img-src 'self' data:; connect-src 'self'; font-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
			w.Write([]byte(docsHTML))
		})
	}
	mux.HandleFunc("GET /docs/init.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte(docsJS))
	})
}
