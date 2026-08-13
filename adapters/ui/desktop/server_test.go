package desktop

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/adapters/embodiment/webavatar"
	"proactive-interaction-engine/adapters/model/local"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	_ AvatarSource        = (*fakeAvatar)(nil)
	_ PermissionService   = (*fakePermissions)(nil)
	_ ControlSubmitter    = (*fakeControls)(nil)
	_ TransportAuthorizer = (*fakeAuthorizer)(nil)
)

func TestNewServerValidatesDependencies(t *testing.T) {
	dependencies := newServerDependencies()
	var nilAvatar *fakeAvatar
	var nilPermissions *fakePermissions
	var nilControls *fakeControls
	var nilAuthorizer *fakeAuthorizer
	var nilClock *engineclock.Fake

	for _, test := range []struct {
		name        string
		subjectID   string
		avatar      AvatarSource
		permissions PermissionService
		controls    ControlSubmitter
		authorizer  TransportAuthorizer
		clock       port.Clock
	}{
		{name: "empty subject", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "nil avatar", subjectID: "user-1", permissions: dependencies.permissions, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "typed nil avatar", subjectID: "user-1", avatar: nilAvatar, permissions: dependencies.permissions, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "nil permissions", subjectID: "user-1", avatar: dependencies.avatar, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "typed nil permissions", subjectID: "user-1", avatar: dependencies.avatar, permissions: nilPermissions, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "nil controls", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "typed nil controls", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: nilControls, authorizer: dependencies.authorizer, clock: dependencies.clock},
		{name: "nil authorizer", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: dependencies.controls, clock: dependencies.clock},
		{name: "typed nil authorizer", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: dependencies.controls, authorizer: nilAuthorizer, clock: dependencies.clock},
		{name: "nil clock", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: dependencies.controls, authorizer: dependencies.authorizer},
		{name: "typed nil clock", subjectID: "user-1", avatar: dependencies.avatar, permissions: dependencies.permissions, controls: dependencies.controls, authorizer: dependencies.authorizer, clock: nilClock},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(test.subjectID, test.avatar, test.permissions, test.controls, test.authorizer, test.clock)
			if server != nil || !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("NewServer() = %#v, %v, want nil and InvalidInput", server, err)
			}
		})
	}
}

func TestEveryRPCAuthenticatesBeforeProtocolOrPayload(t *testing.T) {
	dependencies := newServerDependencies()
	dependencies.authorizer.err = status.Error(codes.Unauthenticated, "missing local token")
	server := newTestServer(t, dependencies)

	if _, err := server.GetState(context.Background(), nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("GetState() code = %s, want Unauthenticated", status.Code(err))
	}
	if _, err := server.SetPermission(context.Background(), nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("SetPermission() code = %s, want Unauthenticated", status.Code(err))
	}
	if _, err := server.RejectInteraction(context.Background(), nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("RejectInteraction() code = %s, want Unauthenticated", status.Code(err))
	}
	watch := newFakeWatchStream(context.Background())
	if err := server.WatchState(nil, watch); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("WatchState() code = %s, want Unauthenticated", status.Code(err))
	}
	if dependencies.authorizer.callCount() != 4 {
		t.Fatalf("Authorize calls = %d, want 4", dependencies.authorizer.callCount())
	}
	if dependencies.permissions.currentCount() != 0 || len(dependencies.controls.snapshot()) != 0 || dependencies.avatar.subscribeCount() != 0 {
		t.Fatal("unauthenticated RPC reached application dependencies")
	}
}

func TestRPCsRejectMissingAndUnsupportedProtocolVersion(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol string
		wantCode codes.Code
	}{
		{name: "missing", wantCode: codes.InvalidArgument},
		{name: "unsupported", protocol: "v2", wantCode: codes.FailedPrecondition},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies := newServerDependencies()
			server := newTestServer(t, dependencies)
			if _, err := server.GetState(context.Background(), &platformv1.GetStateRequest{ProtocolVersion: test.protocol}); status.Code(err) != test.wantCode {
				t.Fatalf("GetState() code = %s, want %s", status.Code(err), test.wantCode)
			}
			if dependencies.permissions.currentCount() != 0 {
				t.Fatal("invalid protocol reached state source")
			}
		})
	}
}

func TestGetStateMapsAllAvatarModesAndPermissionsDeterministically(t *testing.T) {
	wantModes := map[webavatar.Mode]platformv1.AvatarMode{
		webavatar.ModeIdle:          platformv1.AvatarMode_AVATAR_MODE_IDLE,
		webavatar.ModeAttending:     platformv1.AvatarMode_AVATAR_MODE_ATTENDING,
		webavatar.ModeAcknowledging: platformv1.AvatarMode_AVATAR_MODE_ACKNOWLEDGING,
		webavatar.ModeExpressing:    platformv1.AvatarMode_AVATAR_MODE_EXPRESSING,
		webavatar.ModeSpeaking:      platformv1.AvatarMode_AVATAR_MODE_SPEAKING,
	}
	for mode, wantMode := range wantModes {
		t.Run(string(mode), func(t *testing.T) {
			dependencies := newServerDependencies()
			occurredAt := dependencies.clock.Now().Add(-time.Second)
			dependencies.avatar.current = webavatar.Update{
				Revision: 99, Mode: mode, Text: "你好", Speaking: mode == webavatar.ModeSpeaking,
				ActionID: "action-1", OccurredAt: occurredAt,
			}
			updatedAt := dependencies.clock.Now().Add(-time.Minute)
			permissions := privacy.AllPermissions()
			for left, right := 0, len(permissions)-1; left < right; left, right = left+1, right-1 {
				permissions[left], permissions[right] = permissions[right], permissions[left]
			}
			dependencies.permissions.snapshot = privacy.Snapshot{Revision: 17}
			for _, permission := range permissions {
				dependencies.permissions.snapshot.Grants = append(dependencies.permissions.snapshot.Grants, privacy.Grant{
					Permission: permission, Enabled: true, UpdatedAt: updatedAt,
				})
			}

			server := newTestServer(t, dependencies)
			response, err := server.GetState(context.Background(), validGetStateRequest())
			if err != nil {
				t.Fatalf("GetState() error = %v", err)
			}
			state := response.GetState()
			if state.GetRevision() == 0 || len(state.GetProviders()) != 0 {
				t.Fatalf("state = %#v, want server revision and no provider runtimes", state)
			}
			avatar := state.GetAvatar()
			if avatar.GetMode() != wantMode || avatar.GetText() != "你好" || avatar.GetSpeaking() != (mode == webavatar.ModeSpeaking) || avatar.GetActionId() != "action-1" || !avatar.GetOccurredAt().AsTime().Equal(occurredAt) {
				t.Fatalf("avatar = %#v, want mode %s and exact fields", avatar, wantMode)
			}
			if got := permissionKinds(state.GetPermissions()); !reflect.DeepEqual(got, allWirePermissions()) {
				t.Fatalf("permission order = %#v, want %#v", got, allWirePermissions())
			}
			for _, grant := range state.GetPermissions() {
				if !grant.GetEnabled() || !grant.GetUpdatedAt().AsTime().Equal(updatedAt) {
					t.Fatalf("permission grant = %#v, want enabled with exact timestamp", grant)
				}
			}

			dependencies.permissions.snapshot.Grants[0].Enabled = false
			dependencies.avatar.current.Text = "mutated"
			if state.GetAvatar().GetText() != "你好" || !state.GetPermissions()[0].GetEnabled() {
				t.Fatal("returned state aliases mutable sources")
			}
		})
	}
}

func TestGetStateAcceptsRealWebAvatarInitialSnapshot(t *testing.T) {
	dependencies := newServerDependencies()
	avatar, err := webavatar.New(local.NewTemplates(), nil, dependencies.clock)
	if err != nil {
		t.Fatalf("webavatar.New() error = %v", err)
	}
	server, err := NewServer(
		"user-1",
		avatar,
		dependencies.permissions,
		dependencies.controls,
		dependencies.authorizer,
		dependencies.clock,
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	response, err := server.GetState(context.Background(), validGetStateRequest())
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if response.GetState().GetRevision() == 0 || response.GetState().GetAvatar().GetMode() != platformv1.AvatarMode_AVATAR_MODE_IDLE {
		t.Fatalf("GetState() = %#v, want positive composite revision and IDLE avatar", response.GetState())
	}
}

func TestSetPermissionMapsDesiredStateAndReturnsNewCompositeState(t *testing.T) {
	dependencies := newServerDependencies()
	server := newTestServer(t, dependencies)
	initial, err := server.GetState(context.Background(), validGetStateRequest())
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	response, err := server.SetPermission(context.Background(), &platformv1.SetPermissionRequest{
		ProtocolVersion: protocolVersion,
		Permission:      platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE,
		Enabled:         true,
	})
	if err != nil {
		t.Fatalf("SetPermission() error = %v", err)
	}
	if got := dependencies.permissions.changesSnapshot(); !reflect.DeepEqual(got, []privacy.ChangePermission{{Permission: privacy.MicrophoneCapture, Enabled: true}}) {
		t.Fatalf("permission changes = %#v", got)
	}
	if response.GetState().GetRevision() <= initial.GetState().GetRevision() {
		t.Fatalf("SetPermission revision = %d, want greater than initial %d", response.GetState().GetRevision(), initial.GetState().GetRevision())
	}
	if !findWireGrant(t, response.GetState(), platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE).GetEnabled() {
		t.Fatal("SetPermission response did not include desired state")
	}

	for _, invalid := range []platformv1.DesktopPermission{
		platformv1.DesktopPermission_DESKTOP_PERMISSION_UNSPECIFIED,
		platformv1.DesktopPermission(999),
	} {
		if _, err := server.SetPermission(context.Background(), &platformv1.SetPermissionRequest{
			ProtocolVersion: protocolVersion, Permission: invalid, Enabled: true,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("SetPermission(%d) code = %s, want InvalidArgument", invalid, status.Code(err))
		}
	}
	if got := len(dependencies.permissions.changesSnapshot()); got != 1 {
		t.Fatalf("invalid permissions changed state, calls = %d", got)
	}
}

func TestRejectInteractionBuildsServerTimedP0CommandOnly(t *testing.T) {
	dependencies := newServerDependencies()
	server := newTestServer(t, dependencies)
	request := &platformv1.RejectInteractionRequest{
		ProtocolVersion: protocolVersion,
		RequestId:       "reject-1",
		SubjectId:       "user-1",
		TraceId:         "trace-reject",
	}
	response, err := server.RejectInteraction(context.Background(), request)
	if err != nil {
		t.Fatalf("RejectInteraction() error = %v", err)
	}
	if !response.GetAccepted() {
		t.Fatal("RejectInteraction().Accepted = false, want true")
	}
	want := control.Command{
		ID: "reject-1", Kind: control.StopAll, Reason: control.ReasonUserRejected,
		SubjectID: "user-1", OccurredAt: dependencies.clock.Now(), TraceID: "trace-reject",
	}
	if got := dependencies.controls.snapshot(); !reflect.DeepEqual(got, []control.Command{want}) {
		t.Fatalf("SubmitControl commands = %#v, want %#v", got, []control.Command{want})
	}
	if dependencies.permissions.currentCount() != 0 || dependencies.avatar.subscribeCount() != 0 {
		t.Fatal("P0 rejection touched state or observation sources")
	}

	for _, invalid := range []*platformv1.RejectInteractionRequest{
		{ProtocolVersion: protocolVersion, SubjectId: "user-1", TraceId: "trace"},
		{ProtocolVersion: protocolVersion, RequestId: "id", TraceId: "trace"},
		{ProtocolVersion: protocolVersion, RequestId: "id", SubjectId: "other", TraceId: "trace"},
		{ProtocolVersion: protocolVersion, RequestId: "id", SubjectId: "user-1"},
	} {
		if _, err := server.RejectInteraction(context.Background(), invalid); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("RejectInteraction(%#v) code = %s, want InvalidArgument", invalid, status.Code(err))
		}
	}
	if got := len(dependencies.controls.snapshot()); got != 1 {
		t.Fatalf("invalid rejections submitted controls, calls = %d", got)
	}
}

func TestApplicationFaultsMapToStableGRPCCodes(t *testing.T) {
	tests := []struct {
		name     string
		code     fault.Code
		wantCode codes.Code
	}{
		{name: "invalid", code: fault.InvalidInput, wantCode: codes.InvalidArgument},
		{name: "stale", code: fault.StaleInput, wantCode: codes.Aborted},
		{name: "permission", code: fault.PermissionDenied, wantCode: codes.PermissionDenied},
		{name: "deadline", code: fault.DeadlineExceeded, wantCode: codes.DeadlineExceeded},
		{name: "unavailable", code: fault.Unavailable, wantCode: codes.Unavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies := newServerDependencies()
			dependencies.permissions.changeErr = fault.New(test.code, "test permission", errors.New("failure"))
			server := newTestServer(t, dependencies)
			_, err := server.SetPermission(context.Background(), &platformv1.SetPermissionRequest{
				ProtocolVersion: protocolVersion,
				Permission:      platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
				Enabled:         true,
			})
			if status.Code(err) != test.wantCode {
				t.Fatalf("SetPermission() code = %s, want %s", status.Code(err), test.wantCode)
			}
		})
	}
}

func TestWatchStatePublishesLatestCompositeWithoutBlockingPermissionChange(t *testing.T) {
	dependencies := newServerDependencies()
	server := newTestServer(t, dependencies)
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeWatchStream(ctx)
	watchDone := make(chan error, 1)
	go func() {
		watchDone <- server.WatchState(&platformv1.WatchStateRequest{ProtocolVersion: protocolVersion}, stream)
	}()

	initial := stream.next(t)
	dependencies.avatar.publish(webavatar.Update{
		Revision: 50, Mode: webavatar.ModeAttending, ActionID: "stale-avatar", OccurredAt: dependencies.clock.Now(),
	})
	dependencies.avatar.publish(webavatar.Update{
		Revision: 51, Mode: webavatar.ModeSpeaking, Text: "latest", Speaking: true,
		ActionID: "latest-avatar", OccurredAt: dependencies.clock.Now(),
	})
	permissionDone := make(chan error, 1)
	go func() {
		_, err := server.SetPermission(context.Background(), &platformv1.SetPermissionRequest{
			ProtocolVersion: protocolVersion,
			Permission:      platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
			Enabled:         true,
		})
		permissionDone <- err
	}()
	select {
	case err := <-permissionDone:
		if err != nil {
			t.Fatalf("SetPermission() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow WatchState blocked SetPermission")
	}

	latest := stream.next(t)
	for latest.GetAvatar().GetActionId() != "latest-avatar" || !findWireGrant(t, latest, platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE).GetEnabled() {
		latest = stream.next(t)
	}
	if latest.GetRevision() <= initial.GetRevision() {
		t.Fatalf("latest revision = %d, want greater than initial %d", latest.GetRevision(), initial.GetRevision())
	}
	if latest.GetRevision() == 51 || latest.GetRevision() == dependencies.permissions.snapshotCopy().Revision {
		t.Fatal("composite revision reused avatar or privacy source revision")
	}

	cancel()
	select {
	case err := <-watchDone:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("WatchState() code = %s, want Canceled", status.Code(err))
		}
	case <-time.After(time.Second):
		t.Fatal("WatchState did not stop after client disconnect")
	}
	select {
	case <-dependencies.avatar.cancelled:
	case <-time.After(time.Second):
		t.Fatal("WatchState did not release avatar subscription")
	}
}

func validGetStateRequest() *platformv1.GetStateRequest {
	return &platformv1.GetStateRequest{ProtocolVersion: protocolVersion}
}

type serverDependencies struct {
	avatar      *fakeAvatar
	permissions *fakePermissions
	controls    *fakeControls
	authorizer  *fakeAuthorizer
	clock       *engineclock.Fake
}

func newServerDependencies() serverDependencies {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	return serverDependencies{
		avatar: &fakeAvatar{
			current:   webavatar.Update{Revision: 1, Mode: webavatar.ModeIdle, OccurredAt: now},
			cancelled: make(chan struct{}, 8),
		},
		permissions: &fakePermissions{snapshot: disabledPrivacySnapshot()},
		controls:    &fakeControls{},
		authorizer:  &fakeAuthorizer{},
		clock:       clock,
	}
}

func newTestServer(t *testing.T, dependencies serverDependencies) *Server {
	t.Helper()
	server, err := NewServer("user-1", dependencies.avatar, dependencies.permissions, dependencies.controls, dependencies.authorizer, dependencies.clock)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func disabledPrivacySnapshot() privacy.Snapshot {
	permissions := privacy.AllPermissions()
	grants := make([]privacy.Grant, len(permissions))
	for index, permission := range permissions {
		grants[index] = privacy.Grant{Permission: permission}
	}
	return privacy.Snapshot{Grants: grants}
}

func allWirePermissions() []platformv1.DesktopPermission {
	return []platformv1.DesktopPermission{
		platformv1.DesktopPermission_DESKTOP_PERMISSION_CAMERA_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_MICROPHONE_CAPTURE,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_DETECTION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_FACE_LIVENESS,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_IDENTIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEAKER_VERIFICATION,
		platformv1.DesktopPermission_DESKTOP_PERMISSION_SPEECH_TRANSCRIPTION,
	}
}

func permissionKinds(grants []*platformv1.PermissionGrant) []platformv1.DesktopPermission {
	output := make([]platformv1.DesktopPermission, len(grants))
	for index, grant := range grants {
		output[index] = grant.GetPermission()
	}
	return output
}

func findWireGrant(t *testing.T, state *platformv1.DesktopState, permission platformv1.DesktopPermission) *platformv1.PermissionGrant {
	t.Helper()
	for _, grant := range state.GetPermissions() {
		if grant.GetPermission() == permission {
			return grant
		}
	}
	t.Fatalf("state %#v has no permission %s", state, permission)
	return nil
}

type fakeAuthorizer struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (a *fakeAuthorizer) Authorize(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.err
}

func (a *fakeAuthorizer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type fakeAvatar struct {
	mu          sync.Mutex
	current     webavatar.Update
	subscribers map[uint64]chan webavatar.Update
	nextID      uint64
	created     int
	cancelled   chan struct{}
}

func (a *fakeAvatar) Current() webavatar.Update {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current
}

func (a *fakeAvatar) Subscribe() (webavatar.Update, <-chan webavatar.Update, func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subscribers == nil {
		a.subscribers = make(map[uint64]chan webavatar.Update)
	}
	id := a.nextID
	a.nextID++
	a.created++
	updates := make(chan webavatar.Update, 1)
	a.subscribers[id] = updates
	var once sync.Once
	return a.current, updates, func() {
		once.Do(func() {
			a.mu.Lock()
			if channel, exists := a.subscribers[id]; exists {
				delete(a.subscribers, id)
				close(channel)
			}
			a.mu.Unlock()
			a.cancelled <- struct{}{}
		})
	}
}

func (a *fakeAvatar) publish(update webavatar.Update) {
	a.mu.Lock()
	a.current = update
	for _, subscriber := range a.subscribers {
		select {
		case subscriber <- update:
			continue
		default:
		}
		select {
		case <-subscriber:
		default:
		}
		subscriber <- update
	}
	a.mu.Unlock()
}

func (a *fakeAvatar) subscribeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.created
}

type fakePermissions struct {
	mu         sync.Mutex
	snapshot   privacy.Snapshot
	changes    []privacy.ChangePermission
	currentN   int
	currentErr error
	changeErr  error
}

func (p *fakePermissions) Current(ctx context.Context) (privacy.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return privacy.Snapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentN++
	return clonePrivacy(p.snapshot), p.currentErr
}

func (p *fakePermissions) Change(ctx context.Context, command privacy.ChangePermission) (privacy.Snapshot, error) {
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
			p.snapshot.Grants[index].UpdatedAt = time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
		}
	}
	return clonePrivacy(p.snapshot), nil
}

func (p *fakePermissions) currentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentN
}

func (p *fakePermissions) changesSnapshot() []privacy.ChangePermission {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]privacy.ChangePermission(nil), p.changes...)
}

func (p *fakePermissions) snapshotCopy() privacy.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clonePrivacy(p.snapshot)
}

func clonePrivacy(input privacy.Snapshot) privacy.Snapshot {
	input.Grants = append([]privacy.Grant(nil), input.Grants...)
	return input
}

type fakeControls struct {
	mu       sync.Mutex
	commands []control.Command
	err      error
}

func (c *fakeControls) SubmitControl(ctx context.Context, command control.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, command)
	return c.err
}

func (c *fakeControls) snapshot() []control.Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]control.Command(nil), c.commands...)
}

type fakeWatchStream struct {
	ctx  context.Context
	sent chan *platformv1.DesktopState
}

func newFakeWatchStream(ctx context.Context) *fakeWatchStream {
	return &fakeWatchStream{ctx: ctx, sent: make(chan *platformv1.DesktopState, 16)}
}

func (s *fakeWatchStream) Send(response *platformv1.WatchStateResponse) error {
	copy := proto.Clone(response.GetState()).(*platformv1.DesktopState)
	select {
	case s.sent <- copy:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *fakeWatchStream) next(t *testing.T) *platformv1.DesktopState {
	t.Helper()
	select {
	case state := <-s.sent:
		return state
	case <-time.After(time.Second):
		t.Fatal("WatchState did not send state")
		return nil
	}
}

func (s *fakeWatchStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeWatchStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeWatchStream) SetTrailer(metadata.MD)       {}
func (s *fakeWatchStream) Context() context.Context     { return s.ctx }
func (s *fakeWatchStream) SendMsg(any) error            { return nil }
func (s *fakeWatchStream) RecvMsg(any) error            { return nil }

var _ grpc.ServerStreamingServer[platformv1.WatchStateResponse] = (*fakeWatchStream)(nil)
