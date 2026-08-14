package desktopconnect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"proactive-interaction-engine/adapters/embodiment/webavatar"
	"proactive-interaction-engine/adapters/model/local"
	"proactive-interaction-engine/adapters/ui/desktop"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/gen/go/proactive/platform/v1/platformv1connect"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
	"proactive-interaction-engine/internal/runtime/provider"
)

var _ platformv1connect.DesktopControlServiceHandler = (*Bridge)(nil)

func TestNewValidatesDesktopService(t *testing.T) {
	var typedNil *desktop.Server
	for _, service := range []*desktop.Server{nil, typedNil} {
		bridge, err := New(service)
		if bridge != nil || !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("New(%#v) = %#v, %v, want nil and InvalidInput", service, bridge, err)
		}
	}
}

func TestGRPCWebUnaryMethodsReuseDesktopService(t *testing.T) {
	harness := newHarness(t, "")
	client, contentTypes := harness.client()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	getRequest := connect.NewRequest(&platformv1.GetStateRequest{ProtocolVersion: "v1"})
	harness.authorize(getRequest.Header())
	getResponse, err := client.GetState(ctx, getRequest)
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if state := getResponse.Msg.GetState(); state.GetRevision() == 0 || state.GetAvatar().GetMode() != platformv1.AvatarMode_AVATAR_MODE_IDLE {
		t.Fatalf("GetState() = %#v, want positive revision and IDLE avatar", state)
	}

	setRequest := connect.NewRequest(&platformv1.SetPermissionRequest{
		ProtocolVersion: "v1",
		Permission:      platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
		Enabled:         true,
	})
	harness.authorize(setRequest.Header())
	setResponse, err := client.SetPermission(ctx, setRequest)
	if err != nil {
		t.Fatalf("SetPermission() error = %v", err)
	}
	if !wireGrantEnabled(setResponse.Msg.GetState(), platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE) {
		t.Fatal("SetPermission response did not contain enabled camera permission")
	}
	if got := harness.permissions.changesSnapshot(); !reflect.DeepEqual(got, []privacy.ChangePermission{{Permission: privacy.CameraCapture, Enabled: true}}) {
		t.Fatalf("permission changes = %#v", got)
	}

	rejectRequest := connect.NewRequest(&platformv1.RejectInteractionRequest{
		ProtocolVersion: "v1",
		RequestId:       "connect-reject-1",
		SubjectId:       "user-1",
		TraceId:         "connect-trace-1",
	})
	harness.authorize(rejectRequest.Header())
	rejectResponse, err := client.RejectInteraction(ctx, rejectRequest)
	if err != nil {
		t.Fatalf("RejectInteraction() error = %v", err)
	}
	if !rejectResponse.Msg.GetAccepted() {
		t.Fatal("RejectInteraction accepted = false, want true")
	}
	commands := harness.controls.snapshot()
	if len(commands) != 1 || commands[0].Kind != control.StopAll || commands[0].Reason != control.ReasonUserRejected || commands[0].OccurredAt != harness.clock.Now() {
		t.Fatalf("control commands = %#v, want one server-timed USER_REJECTED StopAll", commands)
	}

	seenContentTypes := contentTypes.snapshot()
	if len(seenContentTypes) == 0 {
		t.Fatal("transport recorded no request Content-Type values")
	}
	for _, contentType := range seenContentTypes {
		if !strings.HasPrefix(contentType, "application/grpc-web+proto") {
			t.Fatalf("request Content-Type = %q, want Protobuf gRPC-Web", contentType)
		}
	}
}

func TestBridgePreservesDuplicateAndWrongAuthorizationMetadata(t *testing.T) {
	for _, test := range []struct {
		name          string
		authorization []string
		origins       []string
		wantCode      connect.Code
	}{
		{name: "wrong token", authorization: []string{"Bearer " + alternateBridgeToken()}, origins: []string{"configured"}, wantCode: connect.CodeUnauthenticated},
		{name: "duplicate token", authorization: []string{"Bearer " + bridgeToken(), "Bearer " + bridgeToken()}, origins: []string{"configured"}, wantCode: connect.CodeUnauthenticated},
		{name: "wrong origin", authorization: []string{"Bearer " + bridgeToken()}, origins: []string{"http://127.0.0.1:1"}, wantCode: connect.CodePermissionDenied},
		{name: "duplicate origin", authorization: []string{"Bearer " + bridgeToken()}, origins: []string{"configured", "configured"}, wantCode: connect.CodePermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newHarness(t, "")
			client, _ := harness.client()
			request := connect.NewRequest(&platformv1.GetStateRequest{ProtocolVersion: "v1"})
			for _, value := range test.authorization {
				request.Header().Add("authorization", value)
			}
			for _, value := range test.origins {
				if value == "configured" {
					value = harness.origin
				}
				request.Header().Add("origin", value)
			}
			_, err := client.GetState(context.Background(), request)
			if connect.CodeOf(err) != test.wantCode {
				t.Fatalf("GetState() code = %s, want %s", connect.CodeOf(err), test.wantCode)
			}
		})
	}
}

func TestPeerContextFailsClosedForNonLoopbackOrMalformedRemoteAddress(t *testing.T) {
	for _, remoteAddr := range []string{"192.0.2.10:51000", "malformed", "<empty>"} {
		t.Run(remoteAddr, func(t *testing.T) {
			harness := newHarness(t, remoteAddr)
			client, _ := harness.client()
			request := connect.NewRequest(&platformv1.GetStateRequest{ProtocolVersion: "v1"})
			harness.authorize(request.Header())
			_, err := client.GetState(context.Background(), request)
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("GetState() code = %s, want PermissionDenied", connect.CodeOf(err))
			}
		})
	}
}

func TestBridgeConvertsGRPCStatusToConnectCode(t *testing.T) {
	harness := newHarness(t, "")
	harness.permissions.changeErr = fault.New(fault.Unavailable, "test permission", errors.New("storage offline"))
	client, _ := harness.client()
	request := connect.NewRequest(&platformv1.SetPermissionRequest{
		ProtocolVersion: "v1",
		Permission:      platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
		Enabled:         true,
	})
	harness.authorize(request.Header())
	_, err := client.SetPermission(context.Background(), request)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("SetPermission() code = %s, want Unavailable", connect.CodeOf(err))
	}
}

func TestWatchStateForwardsInitialStateAndCancellation(t *testing.T) {
	harness := newHarness(t, "")
	client, _ := harness.client()
	ctx, cancel := context.WithCancel(context.Background())
	request := connect.NewRequest(&platformv1.WatchStateRequest{ProtocolVersion: "v1"})
	harness.authorize(request.Header())
	stream, err := client.WatchState(ctx, request)
	if err != nil {
		t.Fatalf("WatchState() error = %v", err)
	}
	if !stream.Receive() {
		t.Fatalf("WatchState initial receive error = %v", stream.Err())
	}
	if state := stream.Msg().GetState(); state.GetRevision() == 0 || state.GetAvatar().GetMode() != platformv1.AvatarMode_AVATAR_MODE_IDLE {
		t.Fatalf("WatchState initial = %#v", state)
	}
	cancel()
	if stream.Receive() {
		t.Fatal("WatchState received after cancellation")
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeCanceled {
		t.Fatalf("WatchState cancellation code = %s, want Canceled", code)
	}
}

type bridgeHarness struct {
	server      *httptest.Server
	origin      string
	permissions *bridgePermissions
	controls    *bridgeControls
	clock       *engineclock.Fake
}

func newHarness(t *testing.T, overrideRemoteAddr string) *bridgeHarness {
	t.Helper()
	now := time.Date(2026, time.August, 14, 11, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	avatar, err := webavatar.New(local.NewTemplates(), nil, clock)
	if err != nil {
		t.Fatalf("webavatar.New() error = %v", err)
	}
	permissions := &bridgePermissions{snapshot: bridgeDisabledPermissions()}
	controls := &bridgeControls{}

	testServer := httptest.NewUnstartedServer(nil)
	origin := "http://" + testServer.Listener.Addr().String()
	authorizer, err := desktop.NewLoopbackAuthorizer(bridgeToken(), origin)
	if err != nil {
		t.Fatalf("desktop.NewLoopbackAuthorizer() error = %v", err)
	}
	service, err := desktop.NewServer("user-1", avatar, permissions, bridgeProviders{now: now}, controls, authorizer, clock)
	if err != nil {
		t.Fatalf("desktop.NewServer() error = %v", err)
	}
	bridge, err := New(service)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	path, handler := platformv1connect.NewDesktopControlServiceHandler(bridge)
	handler = PeerContext(handler)
	if overrideRemoteAddr != "" {
		remoteAddr := overrideRemoteAddr
		if remoteAddr == "<empty>" {
			remoteAddr = ""
		}
		next := handler
		handler = http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			request.RemoteAddr = remoteAddr
			next.ServeHTTP(response, request)
		})
	}
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	testServer.Config.Handler = mux
	testServer.Start()
	t.Cleanup(testServer.Close)
	return &bridgeHarness{
		server: testServer, origin: origin, permissions: permissions, controls: controls, clock: clock,
	}
}

func (h *bridgeHarness) client() (platformv1connect.DesktopControlServiceClient, *contentTypeRecorder) {
	recorder := &contentTypeRecorder{next: http.DefaultTransport}
	client := &http.Client{Transport: recorder}
	return platformv1connect.NewDesktopControlServiceClient(client, h.server.URL, connect.WithGRPCWeb()), recorder
}

func (h *bridgeHarness) authorize(header http.Header) {
	header.Set("authorization", "Bearer "+bridgeToken())
	header.Set("origin", h.origin)
}

func bridgeToken() string {
	return "paWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaU"
}

func alternateBridgeToken() string {
	return "WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlo"
}

func bridgeDisabledPermissions() privacy.Snapshot {
	permissions := privacy.AllPermissions()
	grants := make([]privacy.Grant, len(permissions))
	for index, permission := range permissions {
		grants[index] = privacy.Grant{Permission: permission}
	}
	return privacy.Snapshot{Grants: grants}
}

func wireGrantEnabled(state *platformv1.DesktopState, permission platformv1.DesktopPermission) bool {
	for _, grant := range state.GetPermissions() {
		if grant.GetPermission() == permission {
			return grant.GetEnabled()
		}
	}
	return false
}

type bridgePermissions struct {
	mu        sync.Mutex
	snapshot  privacy.Snapshot
	changes   []privacy.ChangePermission
	changeErr error
}

type bridgeProviders struct{ now time.Time }

func (p bridgeProviders) CurrentProviderRuntime() provider.Snapshot {
	return provider.Snapshot{Revision: 1, Providers: []provider.Runtime{{
		ProviderID: "desktop-presence", State: provider.Disabled, Reason: provider.ReasonDisabledByUser, UpdatedAt: p.now,
	}}}
}

func (p bridgeProviders) SubscribeProviderRuntime() (provider.Snapshot, <-chan provider.Snapshot, func()) {
	return p.CurrentProviderRuntime(), make(chan provider.Snapshot), func() {}
}

func (p *bridgePermissions) Current(ctx context.Context) (privacy.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return privacy.Snapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneBridgeSnapshot(p.snapshot), nil
}

func (p *bridgePermissions) Change(ctx context.Context, command privacy.ChangePermission) (privacy.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return privacy.Snapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changes = append(p.changes, command)
	if p.changeErr != nil {
		return privacy.Snapshot{}, p.changeErr
	}
	p.snapshot.Revision++
	for index := range p.snapshot.Grants {
		if p.snapshot.Grants[index].Permission == command.Permission {
			p.snapshot.Grants[index].Enabled = command.Enabled
			p.snapshot.Grants[index].UpdatedAt = time.Date(2026, time.August, 14, 11, 0, 0, 0, time.UTC)
		}
	}
	return cloneBridgeSnapshot(p.snapshot), nil
}

func (p *bridgePermissions) changesSnapshot() []privacy.ChangePermission {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]privacy.ChangePermission(nil), p.changes...)
}

func cloneBridgeSnapshot(snapshot privacy.Snapshot) privacy.Snapshot {
	snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	return snapshot
}

type bridgeControls struct {
	mu       sync.Mutex
	commands []control.Command
}

func (c *bridgeControls) SubmitControl(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, command)
	return nil
}

func (c *bridgeControls) snapshot() []control.Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]control.Command(nil), c.commands...)
}

type contentTypeRecorder struct {
	mu     sync.Mutex
	next   http.RoundTripper
	values []string
}

func (r *contentTypeRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.values = append(r.values, request.Header.Get("Content-Type"))
	r.mu.Unlock()
	return r.next.RoundTrip(request)
}

func (r *contentTypeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.values...)
}
