package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const testOrigin = "http://127.0.0.1:41837"

func TestNewLoopbackAuthorizerValidatesTokenAndOrigin(t *testing.T) {
	validToken := tokenOfLength(32)
	for _, test := range []struct {
		name   string
		token  string
		origin string
	}{
		{name: "empty token", origin: testOrigin},
		{name: "short token", token: tokenOfLength(31), origin: testOrigin},
		{name: "empty origin", token: validToken},
		{name: "non http origin", token: validToken, origin: "https://127.0.0.1:41837"},
		{name: "origin has path", token: validToken, origin: testOrigin + "/desktop"},
		{name: "origin has user info", token: validToken, origin: "http://user@127.0.0.1:41837"},
		{name: "origin has query", token: validToken, origin: testOrigin + "?token=forbidden"},
		{name: "non loopback origin", token: validToken, origin: "http://192.0.2.10:41837"},
		{name: "localhost name is not an explicit IP", token: validToken, origin: "http://localhost:41837"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authorizer, err := NewLoopbackAuthorizer(test.token, test.origin)
			if authorizer != nil || !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("NewLoopbackAuthorizer() = %#v, %v, want nil and InvalidInput", authorizer, err)
			}
		})
	}

	for _, origin := range []string{"http://127.0.0.1:41837", "http://[::1]:41837"} {
		if authorizer, err := NewLoopbackAuthorizer(validToken, origin); err != nil || authorizer == nil {
			t.Fatalf("NewLoopbackAuthorizer(valid, %q) = %#v, %v", origin, authorizer, err)
		}
	}
}

func TestLoopbackAuthorizerRequiresTCPIPLoopbackPeer(t *testing.T) {
	authorizer := newTestLoopbackAuthorizer(t)
	for _, test := range []struct {
		name string
		addr net.Addr
	}{
		{name: "missing peer"},
		{name: "non loopback IPv4", addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 50000}},
		{name: "unix socket denied by default", addr: &net.UnixAddr{Name: "/tmp/desktop.sock", Net: "unix"}},
		{name: "non TCP loopback-shaped address", addr: stringAddr("127.0.0.1:50000")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := authorizedMetadataContext(context.Background(), testToken(), testOrigin)
			if test.addr != nil {
				ctx = peer.NewContext(ctx, &peer.Peer{Addr: test.addr})
			}
			if err := authorizer.Authorize(ctx); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("Authorize() code = %s, want PermissionDenied", status.Code(err))
			}
		})
	}

	for _, ip := range []string{"127.0.0.1", "127.42.7.9", "::1"} {
		t.Run(ip, func(t *testing.T) {
			ctx := fullyAuthorizedContext(testToken(), testOrigin, net.ParseIP(ip))
			if err := authorizer.Authorize(ctx); err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
		})
	}
}

func TestLoopbackAuthorizerRequiresExactlyOneBearerToken(t *testing.T) {
	authorizer := newTestLoopbackAuthorizer(t)
	for _, test := range []struct {
		name   string
		values []string
	}{
		{name: "missing"},
		{name: "duplicate", values: []string{"Bearer " + testToken(), "Bearer " + testToken()}},
		{name: "wrong", values: []string{"Bearer " + alternateTokenOfLength(32)}},
		{name: "lowercase scheme", values: []string{"bearer " + testToken()}},
		{name: "basic scheme", values: []string{"Basic " + testToken()}},
		{name: "leading whitespace", values: []string{" Bearer " + testToken()}},
		{name: "trailing whitespace", values: []string{"Bearer " + testToken() + " "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), loopbackPeer(net.ParseIP("127.0.0.1")))
			pairs := []string{"origin", testOrigin}
			for _, value := range test.values {
				pairs = append(pairs, "authorization", value)
			}
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
			if err := authorizer.Authorize(ctx); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("Authorize() code = %s, want Unauthenticated", status.Code(err))
			}
		})
	}
}

func TestLoopbackAuthorizerRequiresExactlyConfiguredOrigin(t *testing.T) {
	authorizer := newTestLoopbackAuthorizer(t)
	for _, test := range []struct {
		name   string
		values []string
	}{
		{name: "missing"},
		{name: "duplicate", values: []string{testOrigin, testOrigin}},
		{name: "null", values: []string{"null"}},
		{name: "prefix", values: []string{"http://127.0.0.1"}},
		{name: "suffix", values: []string{testOrigin + ".evil.example"}},
		{name: "localhost variant", values: []string{"http://localhost:41837"}},
		{name: "path suffix", values: []string{testOrigin + "/"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), loopbackPeer(net.ParseIP("127.0.0.1")))
			pairs := []string{"authorization", "Bearer " + testToken()}
			for _, value := range test.values {
				pairs = append(pairs, "origin", value)
			}
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
			if err := authorizer.Authorize(ctx); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("Authorize() code = %s, want PermissionDenied", status.Code(err))
			}
		})
	}
}

func TestLoopbackAuthorizerAcceptsExactAuthorizedRequest(t *testing.T) {
	authorizer := newTestLoopbackAuthorizer(t)
	ctx := fullyAuthorizedContext(testToken(), testOrigin, net.ParseIP("127.0.0.1"))
	if err := authorizer.Authorize(ctx); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
}

func TestGenerateTokenUsesExactly32RandomBytesAndBase64URLWithoutPadding(t *testing.T) {
	random := bytes.Repeat([]byte{0xfb}, 32)
	reader := &recordingReader{data: random}
	token, err := GenerateToken(reader)
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if reader.requested != 32 {
		t.Fatalf("GenerateToken read bytes = %d, want 32", reader.requested)
	}
	if strings.Contains(token, "=") {
		t.Fatalf("GenerateToken() = %q, want no padding", token)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	if !bytes.Equal(decoded, random) {
		t.Fatalf("decoded token = %x, want %x", decoded, random)
	}
}

func TestGenerateTokenRejectsNilShortAndFailingReaders(t *testing.T) {
	for _, test := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "nil"},
		{name: "short", reader: bytes.NewReader(bytes.Repeat([]byte{1}, 31))},
		{name: "failure", reader: errorReader{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if token, err := GenerateToken(test.reader); err == nil || token != "" {
				t.Fatalf("GenerateToken() = %q, %v, want empty and error", token, err)
			}
		})
	}
}

func newTestLoopbackAuthorizer(t *testing.T) *LoopbackAuthorizer {
	t.Helper()
	authorizer, err := NewLoopbackAuthorizer(testToken(), testOrigin)
	if err != nil {
		t.Fatalf("NewLoopbackAuthorizer() error = %v", err)
	}
	return authorizer
}

func testToken() string { return tokenOfLength(32) }

func tokenOfLength(length int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, length))
}

func alternateTokenOfLength(length int) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, length))
}

func fullyAuthorizedContext(token, origin string, ip net.IP) context.Context {
	ctx := peer.NewContext(context.Background(), loopbackPeer(ip))
	return authorizedMetadataContext(ctx, token, origin)
}

func authorizedMetadataContext(ctx context.Context, token, origin string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(
		"authorization", "Bearer "+token,
		"origin", origin,
	))
}

func loopbackPeer(ip net.IP) *peer.Peer {
	return &peer.Peer{Addr: &net.TCPAddr{IP: ip, Port: 50000}}
}

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

type recordingReader struct {
	data      []byte
	requested int
}

func (r *recordingReader) Read(output []byte) (int, error) {
	r.requested += len(output)
	return copy(output, r.data), nil
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("random source unavailable") }
