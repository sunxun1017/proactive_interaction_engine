package ingress

import (
	"context"
	"errors"
	"reflect"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/domain/observation"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	reasonLeaseNotFound            = "LEASE_NOT_FOUND"
	reasonLeaseExpired             = "LEASE_EXPIRED"
	reasonProviderUnhealthy        = "PROVIDER_UNHEALTHY"
	reasonProviderNotSelected      = "PROVIDER_NOT_SELECTED"
	reasonProviderInstanceMismatch = "PROVIDER_INSTANCE_MISMATCH"
	reasonCapabilityNotDeclared    = "CAPABILITY_NOT_DECLARED"
	reasonPayloadNotAllowed        = "PAYLOAD_NOT_ALLOWED"
	reasonInactiveSpeech           = "INACTIVE_SPEECH"
	reasonReplyWindowClosed        = "REPLY_WINDOW_CLOSED"
	reasonTTLExpired               = "TTL_EXPIRED"
	reasonEngineStaleInput         = "ENGINE_STALE_INPUT"
	reasonScenarioBlocked          = "SCENARIO_BLOCKED"
)

// ProviderLeaseReader is the immutable registry view consumed by ingress.
type ProviderLeaseReader interface {
	LeaseSnapshot(string) (readiness.ProviderSnapshot, bool)
}

// ObservationSubmitter is the runtime single-writer entry point consumed by
// ingress. The returned application result is not transport receipt state.
type ObservationSubmitter interface {
	SubmitObservation(context.Context, observation.Observation) (application.Result, error)
}

// ReplyWindowReader exposes only the application-owned speech acceptance
// interval needed by the trusted input boundary.
type ReplyWindowReader interface {
	CurrentReplyAcceptanceWindow() (application.ReplyAcceptanceWindow, bool)
}

// ActivationReader exposes only whether the selected scenario's required
// platform capabilities are currently ready.
type ActivationReader interface {
	CurrentActivation() readiness.Activation
}

// Server implements the observation ingress transport boundary.
type Server struct {
	platformv1.UnimplementedObservationIngressServiceServer

	leases     ProviderLeaseReader
	submitter  ObservationSubmitter
	windows    ReplyWindowReader
	activation ActivationReader
	clock      port.Clock
	selected   map[readiness.CapabilityKind]string
}

// NewServer constructs an ingress server for one validated, explicitly
// selected scenario.
func NewServer(
	leases ProviderLeaseReader,
	submitter ObservationSubmitter,
	windows ReplyWindowReader,
	activation ActivationReader,
	clock port.Clock,
	scenario readiness.ScenarioRequirements,
) (*Server, error) {
	if isNilDependency(leases) || isNilDependency(submitter) || isNilDependency(windows) || isNilDependency(activation) || isNilDependency(clock) {
		return nil, errors.New("lease reader, observation submitter, reply window reader, activation reader, and clock are required")
	}
	copied := cloneScenarioRequirements(scenario)
	if err := readiness.ValidateScenario(copied); err != nil {
		return nil, err
	}
	selected := make(map[readiness.CapabilityKind]string, len(copied.Required)+len(copied.Optional))
	for _, requirement := range copied.Required {
		selected[requirement.Kind] = requirement.ProviderID
	}
	for _, optional := range copied.Optional {
		selected[optional.Kind] = optional.ProviderID
	}
	return &Server{
		leases:     leases,
		submitter:  submitter,
		windows:    windows,
		activation: activation,
		clock:      clock,
		selected:   selected,
	}, nil
}

func cloneScenarioRequirements(input readiness.ScenarioRequirements) readiness.ScenarioRequirements {
	output := input
	output.Required = append([]readiness.CapabilityRequirement(nil), input.Required...)
	for index := range output.Required {
		output.Required[index].Compatibility = cloneProviderCompatibility(input.Required[index].Compatibility)
	}
	output.Optional = append([]readiness.OptionalCapability(nil), input.Optional...)
	for index := range output.Optional {
		output.Optional[index].Compatibility = cloneProviderCompatibility(input.Optional[index].Compatibility)
	}
	return output
}

func cloneProviderCompatibility(input readiness.ProviderCompatibility) readiness.ProviderCompatibility {
	input.AllowedPrivacyClasses = append([]readiness.ProviderPrivacyClass(nil), input.AllowedPrivacyClasses...)
	input.AllowedCancellationSemantics = append([]readiness.ProviderCancellationSemantics(nil), input.AllowedCancellationSemantics...)
	input.AllowedDeviceClasses = append([]readiness.ProviderDeviceClass(nil), input.AllowedDeviceClasses...)
	return input
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// Publish validates one transport observation, authorizes its provider lease,
// and submits at most one canonical observation.
func (s *Server) Publish(ctx context.Context, request *platformv1.PublishRequest) (*platformv1.PublishResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	mapped, err := mapPublishRequest(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid observation transport")
	}
	if mapped.rejectedReason != "" {
		return rejected(mapped.input.ID, mapped.rejectedReason), nil
	}
	activation := s.activation.CurrentActivation()
	if activation.Status != readiness.Ready && activation.Status != readiness.Degraded {
		return rejected(mapped.input.ID, reasonScenarioBlocked), nil
	}

	now := s.clock.Now()
	if reason := s.authorize(request.GetProviderLeaseId(), mapped, now); reason != "" {
		return rejected(mapped.input.ID, reason), nil
	}
	if now.After(mapped.input.OccurredAt.Add(mapped.input.TTL)) {
		return stale(mapped.input.ID, reasonTTLExpired), nil
	}
	if err := mapped.input.ValidateAt(now); err != nil {
		if fault.IsCode(err, fault.StaleInput) {
			return stale(mapped.input.ID, reasonTTLExpired), nil
		}
		return nil, status.Error(codes.InvalidArgument, "invalid canonical observation")
	}
	if mapped.speech {
		if !mapped.speechActive {
			return rejected(mapped.input.ID, reasonInactiveSpeech), nil
		}
		window, open := s.windows.CurrentReplyAcceptanceWindow()
		if !replyWindowContains(window, open, mapped.input) {
			return rejected(mapped.input.ID, reasonReplyWindowClosed), nil
		}
	}
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	if _, err := s.submitter.SubmitObservation(ctx, mapped.input); err != nil {
		if fault.IsCode(err, fault.StaleInput) {
			return stale(mapped.input.ID, reasonEngineStaleInput), nil
		}
		return nil, submitStatus(err)
	}
	return accepted(mapped.input.ID), nil
}

func (s *Server) authorize(leaseID string, mapped mappedRequest, now time.Time) string {
	provider, exists := s.leases.LeaseSnapshot(leaseID)
	if !exists {
		return reasonLeaseNotFound
	}
	if provider.Health != readiness.Healthy {
		return reasonProviderUnhealthy
	}
	if !now.Before(provider.LeaseExpiresAt) {
		return reasonLeaseExpired
	}
	selectedProvider, exists := s.selected[mapped.capability]
	if !exists || selectedProvider != provider.ProviderID {
		return reasonProviderNotSelected
	}
	if provider.InstanceID != mapped.input.SourceID {
		return reasonProviderInstanceMismatch
	}
	for _, capability := range provider.Capabilities {
		if capability == mapped.capability {
			return ""
		}
	}
	return reasonCapabilityNotDeclared
}

func replyWindowContains(window application.ReplyAcceptanceWindow, open bool, input observation.Observation) bool {
	return open &&
		window.SubjectID != "" &&
		window.SubjectID == input.SubjectID &&
		window.OpenedAt.Before(window.Deadline) &&
		!input.OccurredAt.Before(window.OpenedAt) &&
		input.OccurredAt.Before(window.Deadline)
}

func accepted(observationID string) *platformv1.PublishResponse {
	return receipt(observationID, platformv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, "")
}

func rejected(observationID, reason string) *platformv1.PublishResponse {
	return receipt(observationID, platformv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, reason)
}

func stale(observationID, reason string) *platformv1.PublishResponse {
	return receipt(observationID, platformv1.ReceiptStatus_RECEIPT_STATUS_STALE, reason)
}

func receipt(observationID string, receiptStatus platformv1.ReceiptStatus, reason string) *platformv1.PublishResponse {
	return &platformv1.PublishResponse{Receipt: &platformv1.ObservationReceipt{
		ObservationId: observationID,
		Status:        receiptStatus,
		ReasonCode:    reason,
	}}
}

func contextStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}

func submitStatus(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case fault.IsCode(err, fault.InvalidInput):
		return status.Error(codes.InvalidArgument, "application rejected canonical observation")
	case fault.IsCode(err, fault.Unavailable):
		return status.Error(codes.Unavailable, "application unavailable")
	case fault.IsCode(err, fault.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "application deadline exceeded")
	default:
		return status.Error(codes.Internal, "application observation processing failed")
	}
}
