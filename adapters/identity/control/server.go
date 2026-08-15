package control

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxConfiguredWatchers           = 128
	maxConfiguredTemplatesPerWork   = 128
	maxConfiguredTemplateBytes      = 4 << 20
	maxConfiguredTotalTemplateBytes = 16 << 20
	maxControlIdentifierLength      = 128
)

const (
	publishVisionOp = "publish vision identity work"
	publishAudioOp  = "publish audio identity work"
)

// ProviderLeaseReader exposes immutable current Registry leases.
type ProviderLeaseReader interface {
	LeaseSnapshot(string) (readiness.ProviderSnapshot, bool)
}

// PermissionReader exposes only global privacy grants. It has no profile,
// enrollment, catalog, or template access.
type PermissionReader interface {
	Current(context.Context) (privacy.Snapshot, error)
}

type selectedProvider struct {
	providerID    string
	compatibility readiness.ProviderCompatibility
}

type leaseBindings map[readiness.CapabilityKind]string

type visionWatcher struct {
	bindings      leaseBindings
	revision      uint64
	desiredActive bool
	updates       chan *platformv1.WatchVisionResponse
	terminal      chan error
}

type audioWatcher struct {
	bindings      leaseBindings
	revision      uint64
	desiredActive bool
	updates       chan *platformv1.WatchAudioResponse
	terminal      chan error
}

// Server owns no goroutine or timer. Each gRPC Watch handler synchronously
// drains one capacity-one latest-state channel until its caller cancels it.
type Server struct {
	platformv1.UnimplementedIdentityWorkerControlServiceServer

	config      Config
	leases      ProviderLeaseReader
	permissions PermissionReader
	clock       port.Clock
	selected    map[readiness.CapabilityKind]selectedProvider

	mu             sync.Mutex
	visionWatchers map[string]*visionWatcher
	audioWatchers  map[string]*audioWatcher
}

// NewServer constructs one control boundary for one validated exact scenario.
// Scenario changes require constructing a new server.
func NewServer(
	config Config,
	leases ProviderLeaseReader,
	permissions PermissionReader,
	clock port.Clock,
	scenario readiness.ScenarioRequirements,
) (*Server, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if isNilControlDependency(leases) || isNilControlDependency(permissions) || isNilControlDependency(clock) {
		return nil, fault.New(fault.InvalidInput, "create identity worker control", errors.New("lease reader, permission reader, and clock are required"))
	}
	if err := readiness.ValidateScenario(scenario); err != nil {
		return nil, err
	}
	selected := make(map[readiness.CapabilityKind]selectedProvider, len(scenario.Required)+len(scenario.Optional))
	for _, requirement := range scenario.Required {
		selected[requirement.Kind] = selectedProvider{providerID: requirement.ProviderID, compatibility: cloneCompatibility(requirement.Compatibility)}
	}
	for _, optional := range scenario.Optional {
		selected[optional.Kind] = selectedProvider{providerID: optional.ProviderID, compatibility: cloneCompatibility(optional.Compatibility)}
	}
	return &Server{
		config: config, leases: leases, permissions: permissions, clock: clock, selected: selected,
		visionWatchers: make(map[string]*visionWatcher), audioWatchers: make(map[string]*audioWatcher),
	}, nil
}

func validateConfig(config Config) error {
	const op = "create identity worker control"
	if config.MaxWatchers <= 0 || config.MaxWatchers > maxConfiguredWatchers {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("maximum watchers must be within [1,%d]", maxConfiguredWatchers))
	}
	if config.MaxTemplatesPerWork <= 0 || config.MaxTemplatesPerWork > maxConfiguredTemplatesPerWork {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("maximum templates per work must be within [1,%d]", maxConfiguredTemplatesPerWork))
	}
	if config.MaxTemplateBytes <= 0 || config.MaxTemplateBytes > maxConfiguredTemplateBytes {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("maximum template bytes must be within [1,%d]", maxConfiguredTemplateBytes))
	}
	if config.MaxTotalTemplateBytes < config.MaxTemplateBytes || config.MaxTotalTemplateBytes > maxConfiguredTotalTemplateBytes {
		return fault.New(fault.InvalidInput, op, fmt.Errorf("maximum total template bytes must be within [%d,%d]", config.MaxTemplateBytes, maxConfiguredTotalTemplateBytes))
	}
	return nil
}

func (s *Server) WatchVision(request *platformv1.WatchVisionRequest, stream platformv1.IdentityWorkerControlService_WatchVisionServer) error {
	if stream == nil {
		return status.Error(codes.InvalidArgument, "vision work stream is required")
	}
	now, err := s.nowForWatch()
	if err != nil {
		return err
	}
	instanceID, bindings, err := normalizeVisionWatch(request)
	if err != nil {
		return err
	}
	if err := s.validateBindings(instanceID, bindings, now); err != nil {
		return err
	}
	watcher, err := s.registerVision(instanceID, bindings)
	if err != nil {
		return err
	}
	defer s.unregisterVision(instanceID, watcher)

	for {
		select {
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		case terminal := <-watcher.terminal:
			return terminal
		case snapshot := <-watcher.updates:
			if err := stream.Send(snapshot); err != nil {
				wipeVisionSnapshot(snapshot)
				return watchSendStatus(err)
			}
			wipeVisionSnapshot(snapshot)
		}
	}
}

func (s *Server) WatchAudio(request *platformv1.WatchAudioRequest, stream platformv1.IdentityWorkerControlService_WatchAudioServer) error {
	if stream == nil {
		return status.Error(codes.InvalidArgument, "audio work stream is required")
	}
	now, err := s.nowForWatch()
	if err != nil {
		return err
	}
	instanceID, bindings, err := normalizeAudioWatch(request)
	if err != nil {
		return err
	}
	if err := s.validateBindings(instanceID, bindings, now); err != nil {
		return err
	}
	watcher, err := s.registerAudio(instanceID, bindings)
	if err != nil {
		return err
	}
	defer s.unregisterAudio(instanceID, watcher)

	for {
		select {
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		case terminal := <-watcher.terminal:
			return terminal
		case snapshot := <-watcher.updates:
			if err := stream.Send(snapshot); err != nil {
				wipeAudioSnapshot(snapshot)
				return watchSendStatus(err)
			}
			wipeAudioSnapshot(snapshot)
		}
	}
}

// PublishVision validates and replaces the latest desired vision state for
// one already-watching device-owner instance.
func (s *Server) PublishVision(ctx context.Context, sourceInstanceID string, work VisionWork) error {
	if err := contextControlFault(ctx, publishVisionOp); err != nil {
		return err
	}
	if !validControlIdentifier(sourceInstanceID) {
		return invalidControl(publishVisionOp, "source instance id is invalid")
	}
	required, err := s.validateVisionWork(work)
	if err != nil {
		return err
	}
	now, err := s.nowForPublish(publishVisionOp)
	if err != nil {
		return err
	}
	if err := validateOpenWindow(publishVisionOp, work.EvidenceWindowID, work.OpenedAt, work.Deadline, now); err != nil {
		return err
	}

	s.mu.Lock()
	watcher := s.visionWatchers[sourceInstanceID]
	s.mu.Unlock()
	if watcher == nil {
		return fault.New(fault.Unavailable, publishVisionOp, errors.New("vision worker is not watching"))
	}
	if err := s.validateActive(ctx, sourceInstanceID, watcher.bindings, required, now); err != nil {
		return err
	}
	snapshot := mapVisionWork(work)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.visionWatchers[sourceInstanceID] != watcher {
		wipeVisionSnapshot(snapshot)
		return fault.New(fault.Unavailable, publishVisionOp, errors.New("vision watcher changed during publish"))
	}
	return s.enqueueVisionLocked(sourceInstanceID, watcher, snapshot, true)
}

// PublishAudio validates and replaces the latest desired audio state for one
// already-watching device-owner instance.
func (s *Server) PublishAudio(ctx context.Context, sourceInstanceID string, work AudioWork) error {
	if err := contextControlFault(ctx, publishAudioOp); err != nil {
		return err
	}
	if !validControlIdentifier(sourceInstanceID) {
		return invalidControl(publishAudioOp, "source instance id is invalid")
	}
	required, err := s.validateAudioWork(work)
	if err != nil {
		return err
	}
	now, err := s.nowForPublish(publishAudioOp)
	if err != nil {
		return err
	}
	if work.Identification != nil {
		err = validateOpenWindow(publishAudioOp, work.Identification.EvidenceWindowID, work.Identification.OpenedAt, work.Identification.Deadline, now)
	} else {
		err = validateOpenWindow(publishAudioOp, work.Verification.EvidenceWindowID, work.Verification.OpenedAt, work.Verification.Deadline, now)
	}
	if err != nil {
		return err
	}

	s.mu.Lock()
	watcher := s.audioWatchers[sourceInstanceID]
	s.mu.Unlock()
	if watcher == nil {
		return fault.New(fault.Unavailable, publishAudioOp, errors.New("audio worker is not watching"))
	}
	if err := s.validateActive(ctx, sourceInstanceID, watcher.bindings, required, now); err != nil {
		return err
	}
	snapshot := mapAudioWork(work)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioWatchers[sourceInstanceID] != watcher {
		wipeAudioSnapshot(snapshot)
		return fault.New(fault.Unavailable, publishAudioOp, errors.New("audio watcher changed during publish"))
	}
	return s.enqueueAudioLocked(sourceInstanceID, watcher, snapshot, true)
}

// ClearVision replaces active desired state with idle without consulting
// privacy, Registry, templates, or catalogs. Repeated idle clears are no-ops.
func (s *Server) ClearVision(sourceInstanceID string) error {
	if !validControlIdentifier(sourceInstanceID) {
		return invalidControl("clear vision identity work", "source instance id is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	watcher := s.visionWatchers[sourceInstanceID]
	if watcher == nil || !watcher.desiredActive {
		return nil
	}
	return s.enqueueVisionLocked(sourceInstanceID, watcher, visionIdleSnapshot(), false)
}

// ClearAudio is the audio equivalent of ClearVision.
func (s *Server) ClearAudio(sourceInstanceID string) error {
	if !validControlIdentifier(sourceInstanceID) {
		return invalidControl("clear audio identity work", "source instance id is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	watcher := s.audioWatchers[sourceInstanceID]
	if watcher == nil || !watcher.desiredActive {
		return nil
	}
	return s.enqueueAudioLocked(sourceInstanceID, watcher, audioIdleSnapshot(), false)
}

func (s *Server) validateVisionWork(work VisionWork) ([]readiness.CapabilityKind, error) {
	if work.Detection == nil && work.Identification == nil && work.Liveness == nil {
		return nil, invalidControl(publishVisionOp, "at least one vision task is required")
	}
	if (work.Identification != nil || work.Liveness != nil) && work.Detection == nil {
		return nil, invalidControl(publishVisionOp, "face identification and liveness require a detection task")
	}
	required := make([]readiness.CapabilityKind, 0, 3)
	if work.Detection != nil {
		required = append(required, readiness.FaceDetection)
	}
	if work.Identification != nil {
		if err := s.validateTemplates(publishVisionOp, work.Identification.Templates); err != nil {
			return nil, err
		}
		required = append(required, readiness.FaceIdentification)
	}
	if work.Liveness != nil {
		required = append(required, readiness.FaceLiveness)
	}
	return required, nil
}

func (s *Server) validateAudioWork(work AudioWork) ([]readiness.CapabilityKind, error) {
	if (work.Identification == nil) == (work.Verification == nil) {
		return nil, invalidControl(publishAudioOp, "exactly one audio task is required")
	}
	if work.Identification != nil {
		if err := s.validateTemplates(publishAudioOp, work.Identification.Templates); err != nil {
			return nil, err
		}
		return []readiness.CapabilityKind{readiness.SpeakerIdentification}, nil
	}
	if !validControlIdentifier(work.Verification.ChallengeID) {
		return nil, invalidControl(publishAudioOp, "verification challenge id is invalid")
	}
	if len(work.Verification.ExpectedEncoded) == 0 || len(work.Verification.ExpectedEncoded) > s.config.MaxTemplateBytes || len(work.Verification.ExpectedEncoded) > s.config.MaxTotalTemplateBytes {
		return nil, invalidControl(publishAudioOp, "verification encoded template exceeds bounds")
	}
	return []readiness.CapabilityKind{readiness.SpeakerVerification}, nil
}

func (s *Server) validateTemplates(op string, templates []IdentificationTemplate) error {
	if len(templates) > s.config.MaxTemplatesPerWork {
		return invalidControl(op, "identification template count exceeds bounds")
	}
	seen := make(map[string]struct{}, len(templates))
	total := 0
	for _, template := range templates {
		if !validControlIdentifier(template.ProfileRef) || len(template.Encoded) == 0 || len(template.Encoded) > s.config.MaxTemplateBytes {
			return invalidControl(op, "identification template is invalid")
		}
		if _, duplicate := seen[template.ProfileRef]; duplicate {
			return invalidControl(op, "identification profile reference is duplicated")
		}
		seen[template.ProfileRef] = struct{}{}
		if total > s.config.MaxTotalTemplateBytes-len(template.Encoded) {
			return invalidControl(op, "identification template bytes exceed total bound")
		}
		total += len(template.Encoded)
	}
	return nil
}

func (s *Server) validateActive(ctx context.Context, instanceID string, bindings leaseBindings, required []readiness.CapabilityKind, now time.Time) error {
	base := readiness.PersonPresence
	if len(required) == 1 && (required[0] == readiness.SpeakerIdentification || required[0] == readiness.SpeakerVerification) {
		base = readiness.VoiceActivity
	}
	activeCapabilities := make([]readiness.CapabilityKind, 0, len(required)+1)
	activeCapabilities = append(activeCapabilities, base)
	activeCapabilities = append(activeCapabilities, required...)
	for _, capability := range activeCapabilities {
		if _, bound := bindings[capability]; !bound {
			return fault.New(fault.PermissionDenied, "authorize identity worker work", errors.New("task capability is not bound"))
		}
		if err := s.validateLease(instanceID, capability, bindings[capability], now); err != nil {
			return fault.New(fault.PermissionDenied, "authorize identity worker work", errors.New("provider lease rejected"))
		}
	}
	permissions, err := s.permissions.Current(ctx)
	if err != nil {
		return permissionControlFault(err)
	}
	for _, capability := range required {
		authorized, err := biometric.CapabilityAuthorized(permissions, capability)
		if err != nil {
			return fault.New(fault.Unavailable, "authorize identity worker work", errors.New("invalid global privacy state"))
		}
		if !authorized {
			return fault.New(fault.PermissionDenied, "authorize identity worker work", errors.New("global capability permission denied"))
		}
	}
	return contextControlFault(ctx, "authorize identity worker work")
}

func (s *Server) validateBindings(instanceID string, bindings leaseBindings, now time.Time) error {
	for capability, leaseID := range bindings {
		if err := s.validateLease(instanceID, capability, leaseID, now); err != nil {
			return status.Error(codes.PermissionDenied, "identity worker lease binding rejected")
		}
	}
	return nil
}

func (s *Server) validateLease(instanceID string, capability readiness.CapabilityKind, leaseID string, now time.Time) error {
	provider, exists := s.leases.LeaseSnapshot(leaseID)
	if !exists || provider.Health != readiness.Healthy || provider.HealthReason != readiness.ProviderHealthReasonNone || !now.Before(provider.LeaseExpiresAt) {
		return errors.New("provider lease is absent, unhealthy, or expired")
	}
	selection, selected := s.selected[capability]
	if !selected || provider.ProviderID != selection.providerID || provider.InstanceID != instanceID {
		return errors.New("provider is not the exact scenario selection")
	}
	if len(provider.Capabilities) != 1 || provider.Capabilities[0] != capability {
		return errors.New("provider lease is not single-capability")
	}
	compatible, err := readiness.ProviderMatchesCompatibility(capability, selection.compatibility, provider)
	if err != nil || !compatible {
		return errors.New("provider is operationally incompatible")
	}
	return nil
}

func (s *Server) registerVision(instanceID string, bindings leaseBindings) (*visionWatcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.visionWatchers[instanceID]; exists {
		return nil, status.Error(codes.AlreadyExists, "vision identity worker is already watching")
	}
	if len(s.visionWatchers)+len(s.audioWatchers) >= s.config.MaxWatchers {
		return nil, status.Error(codes.ResourceExhausted, "identity worker watcher capacity exhausted")
	}
	watcher := &visionWatcher{bindings: bindings, revision: 1, updates: make(chan *platformv1.WatchVisionResponse, 1), terminal: make(chan error, 1)}
	watcher.updates <- &platformv1.WatchVisionResponse{Revision: 1, State: &platformv1.WatchVisionResponse_Idle{Idle: &platformv1.IdentityWorkerIdle{}}}
	s.visionWatchers[instanceID] = watcher
	return watcher, nil
}

func (s *Server) registerAudio(instanceID string, bindings leaseBindings) (*audioWatcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.audioWatchers[instanceID]; exists {
		return nil, status.Error(codes.AlreadyExists, "audio identity worker is already watching")
	}
	if len(s.visionWatchers)+len(s.audioWatchers) >= s.config.MaxWatchers {
		return nil, status.Error(codes.ResourceExhausted, "identity worker watcher capacity exhausted")
	}
	watcher := &audioWatcher{bindings: bindings, revision: 1, updates: make(chan *platformv1.WatchAudioResponse, 1), terminal: make(chan error, 1)}
	watcher.updates <- &platformv1.WatchAudioResponse{Revision: 1, State: &platformv1.WatchAudioResponse_Idle{Idle: &platformv1.IdentityWorkerIdle{}}}
	s.audioWatchers[instanceID] = watcher
	return watcher, nil
}

func (s *Server) unregisterVision(instanceID string, watcher *visionWatcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.visionWatchers[instanceID] != watcher {
		return
	}
	delete(s.visionWatchers, instanceID)
	drainVisionWatcher(watcher)
}

func (s *Server) unregisterAudio(instanceID string, watcher *audioWatcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioWatchers[instanceID] != watcher {
		return
	}
	delete(s.audioWatchers, instanceID)
	drainAudioWatcher(watcher)
}

func (s *Server) enqueueVisionLocked(instanceID string, watcher *visionWatcher, snapshot *platformv1.WatchVisionResponse, active bool) error {
	if watcher.revision == math.MaxUint64 {
		wipeVisionSnapshot(snapshot)
		delete(s.visionWatchers, instanceID)
		drainVisionWatcher(watcher)
		watcher.terminal <- status.Error(codes.ResourceExhausted, "vision work revision exhausted")
		return fault.New(fault.Unavailable, publishVisionOp, errors.New("vision work revision exhausted"))
	}
	watcher.revision++
	snapshot.Revision = watcher.revision
	select {
	case overwritten := <-watcher.updates:
		wipeVisionSnapshot(overwritten)
	default:
	}
	watcher.updates <- snapshot
	watcher.desiredActive = active
	return nil
}

func (s *Server) enqueueAudioLocked(instanceID string, watcher *audioWatcher, snapshot *platformv1.WatchAudioResponse, active bool) error {
	if watcher.revision == math.MaxUint64 {
		wipeAudioSnapshot(snapshot)
		delete(s.audioWatchers, instanceID)
		drainAudioWatcher(watcher)
		watcher.terminal <- status.Error(codes.ResourceExhausted, "audio work revision exhausted")
		return fault.New(fault.Unavailable, publishAudioOp, errors.New("audio work revision exhausted"))
	}
	watcher.revision++
	snapshot.Revision = watcher.revision
	select {
	case overwritten := <-watcher.updates:
		wipeAudioSnapshot(overwritten)
	default:
	}
	watcher.updates <- snapshot
	watcher.desiredActive = active
	return nil
}

func drainVisionWatcher(watcher *visionWatcher) {
	select {
	case snapshot := <-watcher.updates:
		wipeVisionSnapshot(snapshot)
	default:
	}
}

func drainAudioWatcher(watcher *audioWatcher) {
	select {
	case snapshot := <-watcher.updates:
		wipeAudioSnapshot(snapshot)
	default:
	}
}

func normalizeVisionWatch(request *platformv1.WatchVisionRequest) (string, leaseBindings, error) {
	if request == nil {
		return "", nil, status.Error(codes.InvalidArgument, "vision watch request is required")
	}
	allowed := map[readiness.CapabilityKind]struct{}{
		readiness.PersonPresence: {}, readiness.FaceDetection: {}, readiness.FaceIdentification: {}, readiness.FaceLiveness: {},
	}
	instanceID, bindings, err := normalizeWatch(request.GetSourceInstanceId(), request.GetLeaseBindings(), allowed, 4)
	if err != nil {
		return "", nil, err
	}
	if _, present := bindings[readiness.PersonPresence]; !present {
		return "", nil, status.Error(codes.InvalidArgument, "vision watch requires person presence binding")
	}
	return instanceID, bindings, nil
}

func normalizeAudioWatch(request *platformv1.WatchAudioRequest) (string, leaseBindings, error) {
	if request == nil {
		return "", nil, status.Error(codes.InvalidArgument, "audio watch request is required")
	}
	allowed := map[readiness.CapabilityKind]struct{}{
		readiness.VoiceActivity: {}, readiness.SpeakerIdentification: {}, readiness.SpeakerVerification: {},
	}
	instanceID, bindings, err := normalizeWatch(request.GetSourceInstanceId(), request.GetLeaseBindings(), allowed, 3)
	if err != nil {
		return "", nil, err
	}
	if _, present := bindings[readiness.VoiceActivity]; !present {
		return "", nil, status.Error(codes.InvalidArgument, "audio watch requires voice activity binding")
	}
	return instanceID, bindings, nil
}

func normalizeWatch(instanceID string, wire []*platformv1.IdentityWorkerLeaseBinding, allowed map[readiness.CapabilityKind]struct{}, maximum int) (string, leaseBindings, error) {
	if !validControlIdentifier(instanceID) || len(wire) == 0 || len(wire) > maximum {
		return "", nil, status.Error(codes.InvalidArgument, "identity worker watch is invalid or exceeds binding bounds")
	}
	bindings := make(leaseBindings, len(wire))
	seenLeases := make(map[string]struct{}, len(wire))
	for _, binding := range wire {
		if binding == nil || !validControlIdentifier(binding.GetProviderLeaseId()) {
			return "", nil, status.Error(codes.InvalidArgument, "identity worker lease binding is invalid")
		}
		capability, ok := controlCapabilityFromWire(binding.GetCapability())
		if !ok {
			return "", nil, status.Error(codes.InvalidArgument, "identity worker capability is unspecified or unknown")
		}
		if _, ok := allowed[capability]; !ok {
			return "", nil, status.Error(codes.InvalidArgument, "identity worker capability belongs to another stack")
		}
		if _, duplicate := bindings[capability]; duplicate {
			return "", nil, status.Error(codes.InvalidArgument, "identity worker capability binding is duplicated")
		}
		if _, duplicate := seenLeases[binding.GetProviderLeaseId()]; duplicate {
			return "", nil, status.Error(codes.InvalidArgument, "identity worker lease id is duplicated")
		}
		bindings[capability] = binding.GetProviderLeaseId()
		seenLeases[binding.GetProviderLeaseId()] = struct{}{}
	}
	return instanceID, bindings, nil
}

func controlCapabilityFromWire(input platformv1.ServiceCapabilityKind) (readiness.CapabilityKind, bool) {
	switch input {
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE:
		return readiness.PersonPresence, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_DETECTION:
		return readiness.FaceDetection, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_IDENTIFICATION:
		return readiness.FaceIdentification, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_FACE_LIVENESS:
		return readiness.FaceLiveness, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_VOICE_ACTIVITY:
		return readiness.VoiceActivity, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_IDENTIFICATION:
		return readiness.SpeakerIdentification, true
	case platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_SPEAKER_VERIFICATION:
		return readiness.SpeakerVerification, true
	default:
		return "", false
	}
}

func mapVisionWork(work VisionWork) *platformv1.WatchVisionResponse {
	active := &platformv1.VisionActiveWork{
		EvidenceWindowId: work.EvidenceWindowID,
		OpenedAt:         timestamppb.New(work.OpenedAt),
		Deadline:         timestamppb.New(work.Deadline),
	}
	if work.Detection != nil {
		active.Detection = &platformv1.VisionFaceDetectionTask{}
	}
	if work.Identification != nil {
		active.Identification = &platformv1.VisionFaceIdentificationTask{Templates: mapTemplates(work.Identification.Templates)}
	}
	if work.Liveness != nil {
		active.Liveness = &platformv1.VisionFaceLivenessTask{}
	}
	return &platformv1.WatchVisionResponse{State: &platformv1.WatchVisionResponse_Active{Active: active}}
}

func mapAudioWork(work AudioWork) *platformv1.WatchAudioResponse {
	active := &platformv1.AudioActiveWork{}
	if work.Identification != nil {
		active.Task = &platformv1.AudioActiveWork_Identification{Identification: &platformv1.AudioSpeakerIdentificationWork{
			EvidenceWindowId: work.Identification.EvidenceWindowID,
			OpenedAt:         timestamppb.New(work.Identification.OpenedAt),
			Deadline:         timestamppb.New(work.Identification.Deadline),
			Templates:        mapTemplates(work.Identification.Templates),
		}}
	} else {
		active.Task = &platformv1.AudioActiveWork_Verification{Verification: &platformv1.AudioSpeakerVerificationWork{
			VerificationChallengeId: work.Verification.ChallengeID,
			EvidenceWindowId:        work.Verification.EvidenceWindowID,
			OpenedAt:                timestamppb.New(work.Verification.OpenedAt),
			Deadline:                timestamppb.New(work.Verification.Deadline),
			ExpectedEncodedTemplate: append([]byte(nil), work.Verification.ExpectedEncoded...),
		}}
	}
	return &platformv1.WatchAudioResponse{State: &platformv1.WatchAudioResponse_Active{Active: active}}
}

func mapTemplates(input []IdentificationTemplate) []*platformv1.IdentityWorkerIdentificationTemplate {
	output := make([]*platformv1.IdentityWorkerIdentificationTemplate, 0, len(input))
	for _, template := range input {
		output = append(output, &platformv1.IdentityWorkerIdentificationTemplate{
			ProfileRef: template.ProfileRef, EncodedTemplate: append([]byte(nil), template.Encoded...),
		})
	}
	return output
}

func visionIdleSnapshot() *platformv1.WatchVisionResponse {
	return &platformv1.WatchVisionResponse{State: &platformv1.WatchVisionResponse_Idle{Idle: &platformv1.IdentityWorkerIdle{}}}
}

func audioIdleSnapshot() *platformv1.WatchAudioResponse {
	return &platformv1.WatchAudioResponse{State: &platformv1.WatchAudioResponse_Idle{Idle: &platformv1.IdentityWorkerIdle{}}}
}

func wipeVisionSnapshot(snapshot *platformv1.WatchVisionResponse) {
	if snapshot == nil || snapshot.GetActive() == nil || snapshot.GetActive().GetIdentification() == nil {
		return
	}
	wipeTemplates(snapshot.GetActive().GetIdentification().GetTemplates())
}

func wipeAudioSnapshot(snapshot *platformv1.WatchAudioResponse) {
	if snapshot == nil || snapshot.GetActive() == nil {
		return
	}
	if identification := snapshot.GetActive().GetIdentification(); identification != nil {
		wipeTemplates(identification.GetTemplates())
	}
	if verification := snapshot.GetActive().GetVerification(); verification != nil {
		clear(verification.ExpectedEncodedTemplate)
		verification.ExpectedEncodedTemplate = nil
	}
}

func wipeTemplates(templates []*platformv1.IdentityWorkerIdentificationTemplate) {
	for _, template := range templates {
		if template == nil {
			continue
		}
		clear(template.EncodedTemplate)
		template.EncodedTemplate = nil
	}
}

func validateOpenWindow(op, windowID string, openedAt, deadline, now time.Time) error {
	if !validControlIdentifier(windowID) || openedAt.IsZero() || deadline.IsZero() || timestamppb.New(openedAt).CheckValid() != nil || timestamppb.New(deadline).CheckValid() != nil || !openedAt.Before(deadline) {
		return invalidControl(op, "identity work window is invalid")
	}
	if now.Before(openedAt) {
		return invalidControl(op, "identity work window has not opened")
	}
	if !now.Before(deadline) {
		return fault.New(fault.StaleInput, op, errors.New("identity work window is closed"))
	}
	return nil
}

func (s *Server) nowForWatch() (time.Time, error) {
	now := s.clock.Now()
	if now.IsZero() || timestamppb.New(now).CheckValid() != nil {
		return time.Time{}, status.Error(codes.Internal, "identity worker clock returned invalid time")
	}
	return now, nil
}

func (s *Server) nowForPublish(op string) (time.Time, error) {
	now := s.clock.Now()
	if now.IsZero() || timestamppb.New(now).CheckValid() != nil {
		return time.Time{}, fault.New(fault.Unavailable, op, errors.New("identity worker clock returned invalid time"))
	}
	return now, nil
}

func permissionControlFault(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded), fault.IsCode(err, fault.DeadlineExceeded):
		return fault.New(fault.DeadlineExceeded, "read identity worker permissions", err)
	case errors.Is(err, context.Canceled):
		return fault.New(fault.Unavailable, "read identity worker permissions", err)
	case fault.IsCode(err, fault.Unavailable):
		return err
	default:
		return fault.New(fault.Unavailable, "read identity worker permissions", err)
	}
}

func contextControlFault(ctx context.Context, op string) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return fault.New(fault.DeadlineExceeded, op, err)
	case errors.Is(err, context.Canceled):
		return fault.New(fault.Unavailable, op, err)
	default:
		return nil
	}
}

func watchSendStatus(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Unavailable, "identity worker stream send failed")
}

func validControlIdentifier(value string) bool {
	return value != "" && len(value) <= maxControlIdentifierLength && strings.TrimSpace(value) == value
}

func invalidControl(op, message string) error {
	return fault.New(fault.InvalidInput, op, errors.New(message))
}

func cloneCompatibility(input readiness.ProviderCompatibility) readiness.ProviderCompatibility {
	input.AllowedPrivacyClasses = append([]readiness.ProviderPrivacyClass(nil), input.AllowedPrivacyClasses...)
	input.AllowedCancellationSemantics = append([]readiness.ProviderCancellationSemantics(nil), input.AllowedCancellationSemantics...)
	input.AllowedDeviceClasses = append([]readiness.ProviderDeviceClass(nil), input.AllowedDeviceClasses...)
	return input
}

func isNilControlDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}
