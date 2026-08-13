package webpanel

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"proactive-interaction-engine/internal/domain/fault"
)

const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

type asset struct {
	contentType string
	content     []byte
}

// Panel serves a fixed set of immutable in-memory desktop assets.
type Panel struct {
	assets map[string]asset
}

// New renders the fixed index and defensively copies the supplied WASM assets.
func New(token string, wasm, wasmExec []byte) (*Panel, error) {
	const op = "create desktop web panel"
	if strings.TrimSpace(token) == "" {
		return nil, fault.New(fault.InvalidInput, op, errors.New("desktop token is required"))
	}
	if len(wasm) == 0 || len(wasmExec) == 0 {
		return nil, fault.New(fault.InvalidInput, op, errors.New("wasm and wasm_exec assets are required"))
	}
	parsed, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, op, err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, struct{ Token string }{Token: token}); err != nil {
		return nil, fault.New(fault.InvalidInput, op, err)
	}
	return &Panel{assets: map[string]asset{
		"/":                    {contentType: "text/html; charset=utf-8", content: append([]byte(nil), rendered.Bytes()...)},
		"/assets/app.wasm":     {contentType: "application/wasm", content: append([]byte(nil), wasm...)},
		"/assets/wasm_exec.js": {contentType: "text/javascript; charset=utf-8", content: append([]byte(nil), wasmExec...)},
		"/assets/styles.css":   {contentType: "text/css; charset=utf-8", content: []byte(stylesAsset)},
		"/assets/bootstrap.js": {contentType: "text/javascript; charset=utf-8", content: []byte(bootstrapAsset)},
	}}, nil
}

// ServeHTTP serves only exact GET and HEAD requests for fixed assets.
func (p *Panel) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response.Header())
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	escapedPath := request.URL.EscapedPath()
	if escapedPath != request.URL.Path {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	selected, ok := p.assets[request.URL.Path]
	if !ok {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	response.Header().Set("Content-Type", selected.contentType)
	response.Header().Set("Content-Length", strconv.Itoa(len(selected.content)))
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = response.Write(selected.content)
	}
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}
