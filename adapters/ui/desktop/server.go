package desktop

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"

	"proactive-interaction-engine/adapters/embodiment/webavatar"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/control"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/runtime/provider"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const protocolVersion = "v1"

// AvatarSource is the immutable avatar view consumed by the desktop service.
type AvatarSource interface {
	Current() webavatar.Update
	Subscribe() (webavatar.Update, <-chan webavatar.Update, func())
}

// PermissionService reads and changes explicit desired privacy permissions.
type PermissionService interface {
	Current(context.Context) (privacy.Snapshot, error)
	Change(context.Context, privacy.ChangePermission) (privacy.Snapshot, error)
}

// ControlSubmitter is the runtime P0 entry point consumed by this transport.
type ControlSubmitter interface {
	SubmitControl(context.Context, control.Command) error
}

// TransportAuthorizer enforces loopback token and request-origin policy before
// this business service sees request fields.
type TransportAuthorizer interface {
	Authorize(context.Context) error
}

// Server serves one configured local subject.
type Server struct {
	platformv1.UnimplementedDesktopControlServiceServer

	subjectID   string
	avatar      AvatarSource
	permissions PermissionService
	providers   provider.Source
	controls    ControlSubmitter
	authorizer  TransportAuthorizer
	clock       port.Clock

	mu             sync.Mutex
	revision       uint64
	hasSourceTuple bool
	sourceAvatar   uint64
	sourcePrivacy  uint64
	sourceProvider uint64
	permissionSubs map[uint64]chan struct{}
	nextSubID      uint64
}

// NewServer constructs a DesktopControlService for one local subject.
func NewServer(
	subjectID string,
	avatar AvatarSource,
	permissions PermissionService,
	providers provider.Source,
	controls ControlSubmitter,
	authorizer TransportAuthorizer,
	clock port.Clock,
) (*Server, error) {
	const op = "create desktop control server"
	if strings.TrimSpace(subjectID) == "" || subjectID != strings.TrimSpace(subjectID) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("subject id is required"))
	}
	if isNil(avatar) || isNil(permissions) || isNil(providers) || isNil(controls) || isNil(authorizer) || isNil(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("avatar, permissions, providers, controls, authorizer, and clock are required"))
	}
	return &Server{
		subjectID:      subjectID,
		avatar:         avatar,
		permissions:    permissions,
		providers:      providers,
		controls:       controls,
		authorizer:     authorizer,
		clock:          clock,
		permissionSubs: make(map[uint64]chan struct{}),
	}, nil
}

func (s *Server) GetState(ctx context.Context, request *platformv1.GetStateRequest) (*platformv1.GetStateResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := validateProtocol(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	state, err := s.currentState(ctx, s.avatar.Current(), nil, s.providers.CurrentProviderRuntime())
	if err != nil {
		return nil, err
	}
	return &platformv1.GetStateResponse{State: state}, nil
}

func (s *Server) SetPermission(ctx context.Context, request *platformv1.SetPermissionRequest) (*platformv1.SetPermissionResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := validateProtocol(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	permission, err := permissionFromWire(request.GetPermission())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid desktop permission")
	}
	snapshot, err := s.permissions.Change(ctx, privacy.ChangePermission{
		Permission: permission,
		Enabled:    request.GetEnabled(),
	})
	if err != nil {
		return nil, grpcFault(err)
	}
	s.notifyPermissionSubscribers()
	state, err := s.currentState(ctx, s.avatar.Current(), &snapshot, s.providers.CurrentProviderRuntime())
	if err != nil {
		return nil, err
	}
	return &platformv1.SetPermissionResponse{State: state}, nil
}

func (s *Server) RejectInteraction(ctx context.Context, request *platformv1.RejectInteractionRequest) (*platformv1.RejectInteractionResponse, error) {
	if err := s.authorize(ctx); err != nil {
		return nil, err
	}
	if err := validateProtocol(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	if request.GetRequestId() == "" || request.GetRequestId() != strings.TrimSpace(request.GetRequestId()) ||
		request.GetTraceId() == "" || request.GetTraceId() != strings.TrimSpace(request.GetTraceId()) ||
		request.GetSubjectId() != s.subjectID {
		return nil, status.Error(codes.InvalidArgument, "invalid interaction rejection")
	}
	now := s.clock.Now()
	if now.IsZero() {
		return nil, status.Error(codes.Internal, "desktop clock returned invalid time")
	}
	command := control.Command{
		ID:         request.GetRequestId(),
		Kind:       control.StopAll,
		Reason:     control.ReasonUserRejected,
		SubjectID:  s.subjectID,
		OccurredAt: now,
		TraceID:    request.GetTraceId(),
	}
	if err := s.controls.SubmitControl(ctx, command); err != nil {
		return nil, grpcFault(err)
	}
	return &platformv1.RejectInteractionResponse{Accepted: true}, nil
}

func (s *Server) WatchState(request *platformv1.WatchStateRequest, stream platformv1.DesktopControlService_WatchStateServer) error {
	ctx := stream.Context()
	if err := s.authorize(ctx); err != nil {
		return err
	}
	if err := validateProtocol(request.GetProtocolVersion()); err != nil {
		return err
	}

	avatar, avatarUpdates, cancelAvatar := s.avatar.Subscribe()
	defer cancelAvatar()
	providers, providerUpdates, cancelProviders := s.providers.SubscribeProviderRuntime()
	defer cancelProviders()
	permissionUpdates, cancelPermission := s.subscribePermissions()
	defer cancelPermission()

	sendCurrent := func(update webavatar.Update) error {
		state, err := s.currentState(ctx, update, nil, providers)
		if err != nil {
			return err
		}
		if err := stream.Send(&platformv1.WatchStateResponse{State: state}); err != nil {
			return streamError(err)
		}
		return nil
	}
	if err := sendCurrent(avatar); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case update, ok := <-avatarUpdates:
			if !ok {
				return status.Error(codes.Unavailable, "avatar state source closed")
			}
			avatar = update
			if err := sendCurrent(avatar); err != nil {
				return err
			}
		case _, ok := <-permissionUpdates:
			if !ok {
				return status.Error(codes.Unavailable, "permission state source closed")
			}
			avatar = s.avatar.Current()
			if err := sendCurrent(avatar); err != nil {
				return err
			}
		case update, ok := <-providerUpdates:
			if !ok {
				return status.Error(codes.Unavailable, "provider state source closed")
			}
			providers = cloneProviderSnapshot(update)
			if err := sendCurrent(avatar); err != nil {
				return err
			}
		}
	}
}

func (s *Server) currentState(
	ctx context.Context,
	avatar webavatar.Update,
	knownPermissions *privacy.Snapshot,
	providers provider.Snapshot,
) (*platformv1.DesktopState, error) {
	permissions := privacy.Snapshot{}
	if knownPermissions != nil {
		permissions = clonePrivacySnapshot(*knownPermissions)
	} else {
		var err error
		permissions, err = s.permissions.Current(ctx)
		if err != nil {
			return nil, grpcFault(err)
		}
	}
	avatarMessage, err := avatarToWire(avatar)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid avatar state source")
	}
	permissionMessages, err := permissionsToWire(permissions)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid permission state source")
	}
	providerMessages, err := providersToWire(providers)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid provider state source")
	}
	revision := s.compositeRevision(avatar.Revision, permissions.Revision, providers.Revision)
	return &platformv1.DesktopState{
		Revision:    revision,
		Avatar:      avatarMessage,
		Permissions: permissionMessages,
		Providers:   providerMessages,
	}, nil
}

func (s *Server) compositeRevision(avatarRevision, privacyRevision, providerRevision uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasSourceTuple || s.sourceAvatar != avatarRevision || s.sourcePrivacy != privacyRevision || s.sourceProvider != providerRevision {
		s.revision++
		s.hasSourceTuple = true
		s.sourceAvatar = avatarRevision
		s.sourcePrivacy = privacyRevision
		s.sourceProvider = providerRevision
	}
	return s.revision
}

func (s *Server) authorize(ctx context.Context) error {
	if ctx == nil {
		return status.Error(codes.InvalidArgument, "context is required")
	}
	if err := s.authorizer.Authorize(ctx); err != nil {
		if _, ok := status.FromError(err); ok {
			return err
		}
		return status.Error(codes.Unauthenticated, "desktop transport authorization failed")
	}
	return nil
}

func validateProtocol(version string) error {
	if version == "" {
		return status.Error(codes.InvalidArgument, "protocol version is required")
	}
	if version != protocolVersion {
		return status.Error(codes.FailedPrecondition, "unsupported desktop protocol version")
	}
	return nil
}

func grpcFault(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case fault.IsCode(err, fault.InvalidInput):
		return status.Error(codes.InvalidArgument, "application rejected desktop request")
	case fault.IsCode(err, fault.StaleInput):
		return status.Error(codes.Aborted, "desktop state revision conflict")
	case fault.IsCode(err, fault.PermissionDenied):
		return status.Error(codes.PermissionDenied, "desktop operation denied")
	case fault.IsCode(err, fault.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "desktop application deadline exceeded")
	case fault.IsCode(err, fault.Unavailable):
		return status.Error(codes.Unavailable, "desktop application unavailable")
	default:
		return status.Error(codes.Internal, "desktop application operation failed")
	}
}

func streamError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "desktop state stream unavailable")
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func clonePrivacySnapshot(snapshot privacy.Snapshot) privacy.Snapshot {
	snapshot.Grants = append([]privacy.Grant(nil), snapshot.Grants...)
	return snapshot
}

func cloneProviderSnapshot(snapshot provider.Snapshot) provider.Snapshot {
	snapshot.Providers = append([]provider.Runtime(nil), snapshot.Providers...)
	return snapshot
}
