package desktop

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"

	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const tokenBytes = 32

// LoopbackAuthorizer validates the fixed local origin and bearer token for one
// gRPC request. It never reads credentials from query parameters or cookies.
type LoopbackAuthorizer struct {
	token         []byte
	origin        string
	wantBearerLen int
}

// NewLoopbackAuthorizer constructs an authorizer from a generated base64url
// token and an exact HTTP loopback origin with an explicit port.
func NewLoopbackAuthorizer(token, origin string) (*LoopbackAuthorizer, error) {
	const op = "create desktop loopback authorizer"
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) < tokenBytes {
		return nil, fault.New(fault.InvalidInput, op, errors.New("token must contain at least 32 base64url bytes"))
	}
	parsed, err := url.ParseRequestURI(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Port() == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, fault.New(fault.InvalidInput, op, errors.New("origin must be an HTTP loopback IP with an explicit port"))
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, fault.New(fault.InvalidInput, op, errors.New("origin host must be a loopback IP"))
	}
	return &LoopbackAuthorizer{
		token:         append([]byte(nil), []byte(token)...),
		origin:        origin,
		wantBearerLen: len("Bearer ") + len(token),
	}, nil
}

// Authorize validates transport peer and request metadata fail closed.
func (a *LoopbackAuthorizer) Authorize(ctx context.Context) error {
	if ctx == nil {
		return status.Error(codes.PermissionDenied, "desktop peer is not loopback TCP")
	}
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo == nil {
		return status.Error(codes.PermissionDenied, "desktop peer is not loopback TCP")
	}
	address, ok := peerInfo.Addr.(*net.TCPAddr)
	if !ok || address == nil || address.IP == nil || !address.IP.IsLoopback() {
		return status.Error(codes.PermissionDenied, "desktop peer is not loopback TCP")
	}
	incoming, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "desktop bearer token is required")
	}

	origins := incoming.Get("origin")
	if len(origins) != 1 || origins[0] != a.origin {
		return status.Error(codes.PermissionDenied, "desktop request origin is not allowed")
	}
	authorizations := incoming.Get("authorization")
	if len(authorizations) != 1 || !a.matchesBearer(authorizations[0]) {
		return status.Error(codes.Unauthenticated, "desktop bearer token is invalid")
	}
	return nil
}

func (a *LoopbackAuthorizer) matchesBearer(value string) bool {
	expected := make([]byte, 0, a.wantBearerLen)
	expected = append(expected, "Bearer "...)
	expected = append(expected, a.token...)
	provided := []byte(value)
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(provided, expected) == 1
}

// GenerateToken returns a 32-byte random token encoded as unpadded base64url.
func GenerateToken(random io.Reader) (string, error) {
	if random == nil {
		return "", fault.New(fault.InvalidInput, "generate desktop bearer token", errors.New("random reader is required"))
	}
	bytes := make([]byte, tokenBytes)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return "", fmt.Errorf("generate desktop bearer token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
