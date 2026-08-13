package webpanel

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"proactive-interaction-engine/internal/domain/fault"
)

const expectedCSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; connect-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

func TestNewValidatesTokenAndRequiredAssets(t *testing.T) {
	validWASM := []byte{0x00, 0x61, 0x73, 0x6d}
	validExec := []byte("console.log('wasm runtime')")
	for _, test := range []struct {
		name     string
		token    string
		wasm     []byte
		wasmExec []byte
	}{
		{name: "empty token", wasm: validWASM, wasmExec: validExec},
		{name: "blank token", token: " \t", wasm: validWASM, wasmExec: validExec},
		{name: "empty wasm", token: "token", wasmExec: validExec},
		{name: "empty wasm exec", token: "token", wasm: validWASM},
	} {
		t.Run(test.name, func(t *testing.T) {
			panel, err := New(test.token, test.wasm, test.wasmExec)
			if panel != nil || !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("New() = %#v, %v, want nil and InvalidInput", panel, err)
			}
		})
	}
}

func TestIndexIsNoStoreSameOriginBootstrapWithEscapedToken(t *testing.T) {
	token := `local-token-<&"'>`
	panel := newTestPanel(t, token, testWASM(), testWASMExec())
	response := serve(t, panel, http.MethodGet, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", response.Code)
	}
	assertSecurityHeaders(t, response.Header())
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", contentType)
	}
	body := response.Body.String()
	escapedToken := html.EscapeString(token)
	if strings.Contains(body, token) {
		t.Fatalf("HTML contains unescaped raw token %q", token)
	}
	if strings.Count(body, escapedToken) != 1 || !strings.Contains(body, `name="desktop-token"`) {
		t.Fatalf("HTML does not contain exactly one escaped desktop-token meta: %s", body)
	}
	for _, reference := range []string{
		`href="/assets/styles.css"`,
		`src="/assets/wasm_exec.js"`,
		`src="/assets/bootstrap.js"`,
		`data-wasm="/assets/app.wasm"`,
	} {
		if !strings.Contains(body, reference) {
			t.Errorf("HTML missing same-origin reference %s", reference)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") || strings.Contains(body, "//cdn") {
		t.Fatalf("HTML contains external URL: %s", body)
	}
	for name, values := range response.Header() {
		for _, value := range values {
			if strings.Contains(value, token) || strings.Contains(value, escapedToken) {
				t.Fatalf("response header %s leaks token", name)
			}
		}
	}
}

func TestExactAssetPathsUseFixedMIMEAndImmutableContent(t *testing.T) {
	wasm := testWASM()
	wasmExec := testWASMExec()
	panel := newTestPanel(t, "local-token", wasm, wasmExec)
	wasm[0] = 0xff
	wasmExec[0] = 'X'

	for _, test := range []struct {
		path        string
		contentType string
		wantBody    []byte
		nonEmpty    bool
	}{
		{path: "/assets/app.wasm", contentType: "application/wasm", wantBody: testWASM()},
		{path: "/assets/wasm_exec.js", contentType: "text/javascript", wantBody: testWASMExec()},
		{path: "/assets/styles.css", contentType: "text/css", nonEmpty: true},
		{path: "/assets/bootstrap.js", contentType: "text/javascript", nonEmpty: true},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := serve(t, panel, http.MethodGet, test.path)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", test.path, response.Code)
			}
			assertSecurityHeaders(t, response.Header())
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, test.contentType) {
				t.Fatalf("Content-Type = %q, want prefix %q", contentType, test.contentType)
			}
			got := response.Body.Bytes()
			if test.wantBody != nil && !bytes.Equal(got, test.wantBody) {
				t.Fatalf("asset body = %q, want exact %q", got, test.wantBody)
			}
			if test.nonEmpty && len(got) == 0 {
				t.Fatal("built-in asset is empty")
			}
		})
	}
}

func TestHEADReturnsHeadersWithoutBody(t *testing.T) {
	panel := newTestPanel(t, "local-token", testWASM(), testWASMExec())
	for _, path := range []string{"/", "/assets/app.wasm", "/assets/wasm_exec.js", "/assets/styles.css", "/assets/bootstrap.js"} {
		t.Run(path, func(t *testing.T) {
			response := serve(t, panel, http.MethodHead, path)
			if response.Code != http.StatusOK {
				t.Fatalf("HEAD %s status = %d, want 200", path, response.Code)
			}
			assertSecurityHeaders(t, response.Header())
			if response.Body.Len() != 0 {
				t.Fatalf("HEAD %s body = %q, want empty", path, response.Body.Bytes())
			}
		})
	}
}

func TestUnsupportedMethodsReturnAllowWithoutContent(t *testing.T) {
	panel := newTestPanel(t, "local-token", testWASM(), testWASMExec())
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, path := range []string{"/", "/assets/app.wasm"} {
			t.Run(method+" "+path, func(t *testing.T) {
				response := serve(t, panel, method, path)
				if response.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s status = %d, want 405", method, path, response.Code)
				}
				if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Fatalf("Allow = %q, want GET, HEAD", allow)
				}
				assertSecurityHeaders(t, response.Header())
				if response.Body.Len() != 0 {
					t.Fatalf("method rejection body = %q, want empty", response.Body.Bytes())
				}
			})
		}
	}
}

func TestUnknownTraversalAndDirectoryPathsAreNotFound(t *testing.T) {
	panel := newTestPanel(t, "local-token", testWASM(), testWASMExec())
	for _, path := range []string{
		"/unknown",
		"/assets/",
		"/assets",
		"/assets/../app.wasm",
		"/%2e%2e/secret",
		"/assets/%2e%2e/secret",
		"//assets/app.wasm",
	} {
		t.Run(path, func(t *testing.T) {
			response := serve(t, panel, http.MethodGet, path)
			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404", path, response.Code)
			}
			assertSecurityHeaders(t, response.Header())
			if response.Body.Len() != 0 {
				t.Fatalf("not-found body = %q, want empty", response.Body.Bytes())
			}
		})
	}
}

func newTestPanel(t *testing.T, token string, wasm, wasmExec []byte) *Panel {
	t.Helper()
	panel, err := New(token, wasm, wasmExec)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return panel
}

func serve(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := header.Get("Content-Security-Policy"); got != expectedCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, expectedCSP)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func testWASM() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
}

func testWASMExec() []byte {
	return []byte("globalThis.Go = class Go {};")
}
