package ingress

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxTrackedSources = 1024

// Config explicitly bounds provider sequence state retained by ingress.
type Config struct {
	MaxTrackedSources int
}

// ProviderLeaseReader is the immutable Registry view consumed by ingress.
type ProviderLeaseReader interface {
	LeaseSnapshot(string) (readiness.ProviderSnapshot, bool)
}

// PermissionReader exposes only the current global privacy grants. Identity
// ingress never reads per-profile consent or enrollment state.
type PermissionReader interface {
	Current(context.Context) (privacy.Snapshot, error)
}

// IdentificationEvidenceSubmitter is the application-owned identification
// window boundary consumed by ingress.
type IdentificationEvidenceSubmitter interface {
	SubmitFaceDetectionAt(identity.FaceDetectionFragment, time.Time) (identity.FragmentReceipt, error)
	SubmitFaceIdentificationAt(identity.FaceIdentificationFragment, time.Time) (identity.FragmentReceipt, error)
	SubmitFaceLivenessAt(identity.FaceLivenessFragment, time.Time) (identity.FragmentReceipt, error)
	SubmitSpeakerIdentificationAt(identity.SpeakerIdentificationFragment, time.Time) (identity.FragmentReceipt, error)
}

// SpeakerVerificationEvidenceSubmitter is the application-owned challenge
// boundary. The challenge, not the worker request, owns the expected profile.
type SpeakerVerificationEvidenceSubmitter interface {
	SubmitSpeakerVerificationAt(identity.SpeakerVerificationFragment, time.Time) (identity.FragmentReceipt, error)
}

type selectedProvider struct {
	providerID    string
	compatibility readiness.ProviderCompatibility
}

type sourceStreamKey struct {
	leaseID    string
	capability readiness.CapabilityKind
}

type sourcePosition struct {
	sequence   uint64
	windowID   string
	fragmentID string
}

// Server implements the strongly typed identity evidence transport boundary.
type Server struct {
	platformv1.UnimplementedIdentityEvidenceIngressServiceServer

	config         Config
	leases         ProviderLeaseReader
	permissions    PermissionReader
	identification IdentificationEvidenceSubmitter
	verification   SpeakerVerificationEvidenceSubmitter
	clock          port.Clock
	selected       map[readiness.CapabilityKind]selectedProvider

	sequenceMu sync.Mutex
	positions  map[sourceStreamKey]sourcePosition
}

// NewServer constructs ingress for one validated, explicitly selected
// scenario. A new server is required when the active scenario changes.
func NewServer(
	config Config,
	leases ProviderLeaseReader,
	permissions PermissionReader,
	identification IdentificationEvidenceSubmitter,
	verification SpeakerVerificationEvidenceSubmitter,
	clock port.Clock,
	scenario readiness.ScenarioRequirements,
) (*Server, error) {
	if config.MaxTrackedSources <= 0 || config.MaxTrackedSources > maxTrackedSources {
		return nil, fmt.Errorf("maximum tracked identity sources must be within [1,%d]", maxTrackedSources)
	}
	if isNilDependency(leases) || isNilDependency(permissions) || isNilDependency(identification) || isNilDependency(verification) || isNilDependency(clock) {
		return nil, errors.New("lease reader, permission reader, evidence submitters, and clock are required")
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
		config: config, leases: leases, permissions: permissions, identification: identification,
		verification: verification, clock: clock, selected: selected,
		positions: make(map[sourceStreamKey]sourcePosition, config.MaxTrackedSources),
	}, nil
}

func (s *Server) PublishFaceDetectionEvidence(ctx context.Context, request *platformv1.PublishFaceDetectionEvidenceRequest) (*platformv1.PublishFaceDetectionEvidenceResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	mapped, err := mapFaceDetection(request, now)
	if err != nil {
		receipt, transportErr := mapTransportError(request.GetMetadata(), err)
		if transportErr != nil {
			return nil, transportErr
		}
		return &platformv1.PublishFaceDetectionEvidenceResponse{Receipt: receipt}, nil
	}
	if receipt, preflightErr := s.preflight(ctx, mapped.metadata, mapped.capability, now); preflightErr != nil {
		return nil, preflightErr
	} else if receipt != nil {
		return &platformv1.PublishFaceDetectionEvidenceResponse{Receipt: receipt}, nil
	}
	fragment := identity.FaceDetectionFragment{
		WindowToken: mapped.metadata.evidenceWindowID, FragmentID: mapped.metadata.fragmentID,
		OccurredAt: mapped.metadata.occurredAt, FacesObserved: mapped.facesObserved,
	}
	receipt, err := s.submitSequenced(ctx, mapped.metadata, mapped.capability, now, func() (identity.FragmentReceipt, error) {
		return s.identification.SubmitFaceDetectionAt(fragment, now)
	})
	if err != nil {
		return nil, err
	}
	return &platformv1.PublishFaceDetectionEvidenceResponse{Receipt: receipt}, nil
}

func (s *Server) PublishFaceIdentificationEvidence(ctx context.Context, request *platformv1.PublishFaceIdentificationEvidenceRequest) (*platformv1.PublishFaceIdentificationEvidenceResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	mapped, err := mapFaceIdentification(request, now)
	if err != nil {
		receipt, transportErr := mapTransportError(request.GetMetadata(), err)
		if transportErr != nil {
			return nil, transportErr
		}
		return &platformv1.PublishFaceIdentificationEvidenceResponse{Receipt: receipt}, nil
	}
	if receipt, preflightErr := s.preflight(ctx, mapped.metadata, mapped.capability, now); preflightErr != nil {
		return nil, preflightErr
	} else if receipt != nil {
		return &platformv1.PublishFaceIdentificationEvidenceResponse{Receipt: receipt}, nil
	}
	fragment := identity.FaceIdentificationFragment{
		WindowToken: mapped.metadata.evidenceWindowID, FragmentID: mapped.metadata.fragmentID,
		OccurredAt: mapped.metadata.occurredAt, Candidates: mapped.candidates,
	}
	receipt, err := s.submitSequenced(ctx, mapped.metadata, mapped.capability, now, func() (identity.FragmentReceipt, error) {
		return s.identification.SubmitFaceIdentificationAt(fragment, now)
	})
	if err != nil {
		return nil, err
	}
	return &platformv1.PublishFaceIdentificationEvidenceResponse{Receipt: receipt}, nil
}

func (s *Server) PublishFaceLivenessEvidence(ctx context.Context, request *platformv1.PublishFaceLivenessEvidenceRequest) (*platformv1.PublishFaceLivenessEvidenceResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	mapped, err := mapFaceLiveness(request, now)
	if err != nil {
		receipt, transportErr := mapTransportError(request.GetMetadata(), err)
		if transportErr != nil {
			return nil, transportErr
		}
		return &platformv1.PublishFaceLivenessEvidenceResponse{Receipt: receipt}, nil
	}
	if receipt, preflightErr := s.preflight(ctx, mapped.metadata, mapped.capability, now); preflightErr != nil {
		return nil, preflightErr
	} else if receipt != nil {
		return &platformv1.PublishFaceLivenessEvidenceResponse{Receipt: receipt}, nil
	}
	fragment := identity.FaceLivenessFragment{
		WindowToken: mapped.metadata.evidenceWindowID, FragmentID: mapped.metadata.fragmentID,
		OccurredAt: mapped.metadata.occurredAt, State: mapped.state,
	}
	receipt, err := s.submitSequenced(ctx, mapped.metadata, mapped.capability, now, func() (identity.FragmentReceipt, error) {
		return s.identification.SubmitFaceLivenessAt(fragment, now)
	})
	if err != nil {
		return nil, err
	}
	return &platformv1.PublishFaceLivenessEvidenceResponse{Receipt: receipt}, nil
}

func (s *Server) PublishSpeakerIdentificationEvidence(ctx context.Context, request *platformv1.PublishSpeakerIdentificationEvidenceRequest) (*platformv1.PublishSpeakerIdentificationEvidenceResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	mapped, err := mapSpeakerIdentification(request, now)
	if err != nil {
		receipt, transportErr := mapTransportError(request.GetMetadata(), err)
		if transportErr != nil {
			return nil, transportErr
		}
		return &platformv1.PublishSpeakerIdentificationEvidenceResponse{Receipt: receipt}, nil
	}
	if receipt, preflightErr := s.preflight(ctx, mapped.metadata, mapped.capability, now); preflightErr != nil {
		return nil, preflightErr
	} else if receipt != nil {
		return &platformv1.PublishSpeakerIdentificationEvidenceResponse{Receipt: receipt}, nil
	}
	fragment := identity.SpeakerIdentificationFragment{
		WindowToken: mapped.metadata.evidenceWindowID, FragmentID: mapped.metadata.fragmentID,
		OccurredAt: mapped.metadata.occurredAt, Candidates: mapped.candidates,
	}
	receipt, err := s.submitSequenced(ctx, mapped.metadata, mapped.capability, now, func() (identity.FragmentReceipt, error) {
		return s.identification.SubmitSpeakerIdentificationAt(fragment, now)
	})
	if err != nil {
		return nil, err
	}
	return &platformv1.PublishSpeakerIdentificationEvidenceResponse{Receipt: receipt}, nil
}

func (s *Server) PublishSpeakerVerificationEvidence(ctx context.Context, request *platformv1.PublishSpeakerVerificationEvidenceRequest) (*platformv1.PublishSpeakerVerificationEvidenceResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	mapped, err := mapSpeakerVerification(request, now)
	if err != nil {
		receipt, transportErr := mapTransportError(request.GetMetadata(), err)
		if transportErr != nil {
			return nil, transportErr
		}
		return &platformv1.PublishSpeakerVerificationEvidenceResponse{Receipt: receipt}, nil
	}
	if receipt, preflightErr := s.preflight(ctx, mapped.metadata, mapped.capability, now); preflightErr != nil {
		return nil, preflightErr
	} else if receipt != nil {
		return &platformv1.PublishSpeakerVerificationEvidenceResponse{Receipt: receipt}, nil
	}
	fragment := identity.SpeakerVerificationFragment{
		ChallengeID: mapped.challengeID, WindowToken: mapped.metadata.evidenceWindowID,
		FragmentID: mapped.metadata.fragmentID, OccurredAt: mapped.metadata.occurredAt,
		Candidate: identity.SpeakerVerificationSubmissionCandidate{
			ID: mapped.candidate.ID, Score: mapped.candidate.Score, ModelVersion: mapped.candidate.ModelVersion,
		},
	}
	receipt, err := s.submitSequenced(ctx, mapped.metadata, mapped.capability, now, func() (identity.FragmentReceipt, error) {
		return s.verification.SubmitSpeakerVerificationAt(fragment, now)
	})
	if err != nil {
		return nil, err
	}
	return &platformv1.PublishSpeakerVerificationEvidenceResponse{Receipt: receipt}, nil
}

func (s *Server) preflight(ctx context.Context, metadata evidenceMetadata, capability readiness.CapabilityKind, now time.Time) (*platformv1.IdentityEvidenceReceipt, error) {
	if reason := s.authorize(metadata, capability, now); reason != platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE {
		return rejectedReceipt(metadata.fragmentID, reason), nil
	}
	permissions, err := s.permissions.Current(ctx)
	if err != nil {
		return nil, permissionReadStatus(err)
	}
	authorized, err := biometric.CapabilityAuthorized(permissions, capability)
	if err != nil {
		return nil, status.Error(codes.Internal, "invalid global permission state")
	}
	if !authorized {
		return rejectedReceipt(metadata.fragmentID, platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_POLICY_REJECTED), nil
	}
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Server) authorize(metadata evidenceMetadata, capability readiness.CapabilityKind, now time.Time) platformv1.IdentityEvidenceReceiptReason {
	provider, exists := s.leases.LeaseSnapshot(metadata.providerLeaseID)
	if !exists || provider.Health != readiness.Healthy || !now.Before(provider.LeaseExpiresAt) {
		return platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_LEASE_REJECTED
	}
	selection, selected := s.selected[capability]
	if !selected || provider.ProviderID != selection.providerID {
		return platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_PROVIDER_NOT_SELECTED
	}
	if provider.InstanceID != metadata.sourceInstanceID || !declaresCapability(provider.Capabilities, capability) {
		return platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_LEASE_REJECTED
	}
	compatible, err := readiness.ProviderMatchesCompatibility(capability, selection.compatibility, provider)
	if err != nil || !compatible {
		return platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_LEASE_REJECTED
	}
	return platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE
}

func (s *Server) submitSequenced(
	ctx context.Context,
	metadata evidenceMetadata,
	capability readiness.CapabilityKind,
	now time.Time,
	submit func() (identity.FragmentReceipt, error),
) (*platformv1.IdentityEvidenceReceipt, error) {
	key := sourceStreamKey{leaseID: metadata.providerLeaseID, capability: capability}
	// Submitters are local, bounded application coordinators. Holding this lock
	// across submission makes the sequence check and accepted state atomic.
	s.sequenceMu.Lock()
	defer s.sequenceMu.Unlock()

	position, tracked := s.positions[key]
	if tracked && metadata.sourceSeq < position.sequence {
		return staleReceipt(metadata.fragmentID), nil
	}
	sameSequence := tracked && metadata.sourceSeq == position.sequence
	if sameSequence && (metadata.evidenceWindowID != position.windowID || metadata.fragmentID != position.fragmentID) {
		return staleReceipt(metadata.fragmentID), nil
	}
	if !tracked && len(s.positions) >= s.config.MaxTrackedSources {
		s.pruneExpiredPositionsLocked(now)
		if len(s.positions) >= s.config.MaxTrackedSources {
			return nil, status.Error(codes.Unavailable, "identity ingress source capacity exhausted")
		}
	}
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	applicationReceipt, err := submit()
	if err != nil {
		return mapApplicationError(metadata.fragmentID, err)
	}
	if applicationReceipt.FragmentID != metadata.fragmentID {
		return nil, status.Error(codes.Internal, "identity application returned mismatched receipt")
	}
	var receipt *platformv1.IdentityEvidenceReceipt
	switch applicationReceipt.Status {
	case identity.FragmentAccepted:
		if sameSequence {
			return nil, status.Error(codes.Internal, "identity application accepted a repeated source sequence")
		}
		receipt = acceptedReceipt(metadata.fragmentID)
	case identity.FragmentDuplicate:
		receipt = duplicateReceipt(metadata.fragmentID)
	default:
		return nil, status.Error(codes.Internal, "identity application returned an unknown receipt")
	}
	s.positions[key] = sourcePosition{sequence: metadata.sourceSeq, windowID: metadata.evidenceWindowID, fragmentID: metadata.fragmentID}
	return receipt, nil
}

func (s *Server) pruneExpiredPositionsLocked(now time.Time) {
	for key := range s.positions {
		provider, exists := s.leases.LeaseSnapshot(key.leaseID)
		if !exists || !now.Before(provider.LeaseExpiresAt) {
			delete(s.positions, key)
		}
	}
}

func mapTransportError(metadata *platformv1.IdentityEvidenceMetadata, err error) (*platformv1.IdentityEvidenceReceipt, error) {
	if fault.IsCode(err, fault.StaleInput) {
		return staleReceipt(metadata.GetFragmentId()), nil
	}
	return nil, status.Error(codes.InvalidArgument, "invalid identity evidence transport")
}

func mapApplicationError(fragmentID string, err error) (*platformv1.IdentityEvidenceReceipt, error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, status.FromContextError(err).Err()
	case fault.IsCode(err, fault.StaleInput):
		return staleReceipt(fragmentID), nil
	case fault.IsCode(err, fault.InvalidInput):
		return rejectedReceipt(fragmentID, platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_INVALID), nil
	case fault.IsCode(err, fault.PolicyBlocked), fault.IsCode(err, fault.PermissionDenied):
		return rejectedReceipt(fragmentID, platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_POLICY_REJECTED), nil
	case fault.IsCode(err, fault.Unavailable):
		return nil, status.Error(codes.Unavailable, "identity application unavailable")
	case fault.IsCode(err, fault.DeadlineExceeded):
		return nil, status.Error(codes.DeadlineExceeded, "identity application deadline exceeded")
	default:
		return nil, status.Error(codes.Internal, "identity application submission failed")
	}
}

func permissionReadStatus(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case fault.IsCode(err, fault.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "global permission read deadline exceeded")
	case fault.IsCode(err, fault.Unavailable):
		return status.Error(codes.Unavailable, "global permission state unavailable")
	default:
		return status.Error(codes.Internal, "global permission read failed")
	}
}

func acceptedReceipt(fragmentID string) *platformv1.IdentityEvidenceReceipt {
	return identityReceipt(fragmentID,
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_ACCEPTED,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_NONE)
}

func duplicateReceipt(fragmentID string) *platformv1.IdentityEvidenceReceipt {
	return identityReceipt(fragmentID,
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_DUPLICATE,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_DUPLICATE)
}

func staleReceipt(fragmentID string) *platformv1.IdentityEvidenceReceipt {
	return identityReceipt(fragmentID,
		platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_STALE,
		platformv1.IdentityEvidenceReceiptReason_IDENTITY_EVIDENCE_RECEIPT_REASON_STALE)
}

func rejectedReceipt(fragmentID string, reason platformv1.IdentityEvidenceReceiptReason) *platformv1.IdentityEvidenceReceipt {
	return identityReceipt(fragmentID, platformv1.IdentityEvidenceReceiptStatus_IDENTITY_EVIDENCE_RECEIPT_STATUS_REJECTED, reason)
}

func identityReceipt(fragmentID string, receiptStatus platformv1.IdentityEvidenceReceiptStatus, reason platformv1.IdentityEvidenceReceiptReason) *platformv1.IdentityEvidenceReceipt {
	return &platformv1.IdentityEvidenceReceipt{FragmentId: fragmentID, Status: receiptStatus, Reason: reason}
}

func declaresCapability(capabilities []readiness.CapabilityKind, expected readiness.CapabilityKind) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func cloneCompatibility(input readiness.ProviderCompatibility) readiness.ProviderCompatibility {
	input.AllowedPrivacyClasses = append([]readiness.ProviderPrivacyClass(nil), input.AllowedPrivacyClasses...)
	input.AllowedCancellationSemantics = append([]readiness.ProviderCancellationSemantics(nil), input.AllowedCancellationSemantics...)
	input.AllowedDeviceClasses = append([]readiness.ProviderDeviceClass(nil), input.AllowedDeviceClasses...)
	return input
}

func contextStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

func isNilDependency(value any) bool {
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
