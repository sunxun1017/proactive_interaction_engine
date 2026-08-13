package ingress

import (
	"errors"
	"math"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/observation"
)

type mappedRequest struct {
	input          observation.Observation
	capability     readiness.CapabilityKind
	speech         bool
	speechActive   bool
	rejectedReason string
}

func mapPublishRequest(request *platformv1.PublishRequest) (mappedRequest, error) {
	if request == nil || request.GetProviderLeaseId() == "" || request.GetObservation() == nil {
		return mappedRequest{}, errors.New("request, provider lease, and observation are required")
	}
	wire := request.GetObservation()
	if wire.GetId() == "" || wire.GetSourceId() == "" || wire.GetSourceSeq() == 0 || wire.GetSubjectId() == "" || wire.GetTraceId() == "" {
		return mappedRequest{}, errors.New("observation metadata is incomplete")
	}
	if wire.GetOccurredAt() == nil || wire.GetOccurredAt().CheckValid() != nil {
		return mappedRequest{}, errors.New("observation timestamp is invalid")
	}
	if wire.GetTtl() == nil || wire.GetTtl().CheckValid() != nil || wire.GetTtl().AsDuration() <= 0 {
		return mappedRequest{}, errors.New("observation ttl is invalid")
	}
	confidence := wire.GetConfidence()
	if math.IsNaN(float64(confidence)) || math.IsInf(float64(confidence), 0) || confidence < 0 || confidence > 1 {
		return mappedRequest{}, errors.New("observation confidence is invalid")
	}

	input := observation.Observation{
		ID:         wire.GetId(),
		SourceID:   wire.GetSourceId(),
		SourceSeq:  wire.GetSourceSeq(),
		OccurredAt: wire.GetOccurredAt().AsTime(),
		TTL:        wire.GetTtl().AsDuration(),
		SubjectID:  wire.GetSubjectId(),
		Confidence: confidence,
		TraceID:    wire.GetTraceId(),
	}
	mapped := mappedRequest{input: input}
	switch payload := wire.GetPayload().(type) {
	case *platformv1.ObservationEnvelope_PersonPresence:
		if payload.PersonPresence == nil {
			return mappedRequest{}, errors.New("person presence payload is required")
		}
		mapped.capability = readiness.PersonPresence
		mapped.input.PersonPresence = &observation.PersonPresence{Present: payload.PersonPresence.GetPresent()}
	case *platformv1.ObservationEnvelope_UserBusy:
		if payload.UserBusy == nil {
			return mappedRequest{}, errors.New("user busy payload is required")
		}
		busyReason, ok := mapBusyReason(payload.UserBusy.GetBusy(), payload.UserBusy.GetReason())
		if !ok {
			return mappedRequest{}, errors.New("user busy state and reason are inconsistent")
		}
		mapped.capability = readiness.BusyState
		mapped.input.UserBusy = &observation.UserBusy{Busy: payload.UserBusy.GetBusy(), Reason: busyReason}
	case *platformv1.ObservationEnvelope_SpeechActivity:
		if payload.SpeechActivity == nil {
			return mappedRequest{}, errors.New("speech activity payload is required")
		}
		mapped.capability = readiness.VoiceActivity
		mapped.speech = true
		mapped.speechActive = payload.SpeechActivity.GetActive()
		mapped.input.Confidence = 1
		mapped.input.UserReply = &observation.UserReply{}
	case *platformv1.ObservationEnvelope_UserReply:
		if payload.UserReply == nil {
			return mappedRequest{}, errors.New("user reply payload is required")
		}
		mapped.rejectedReason = reasonPayloadNotAllowed
	case *platformv1.ObservationEnvelope_UserControl:
		if payload.UserControl == nil {
			return mappedRequest{}, errors.New("user control payload is required")
		}
		mapped.rejectedReason = reasonPayloadNotAllowed
	case *platformv1.ObservationEnvelope_QuietMode:
		if payload.QuietMode == nil {
			return mappedRequest{}, errors.New("quiet mode payload is required")
		}
		mapped.rejectedReason = reasonPayloadNotAllowed
	case *platformv1.ObservationEnvelope_DeviceCondition:
		if payload.DeviceCondition == nil {
			return mappedRequest{}, errors.New("device condition payload is required")
		}
		mapped.rejectedReason = reasonPayloadNotAllowed
	case nil:
		return mappedRequest{}, errors.New("observation payload is required")
	default:
		mapped.rejectedReason = reasonPayloadNotAllowed
	}
	return mapped, nil
}

func mapBusyReason(busy bool, reason platformv1.BusyReason) (observation.BusyReason, bool) {
	if !busy {
		if reason != platformv1.BusyReason_BUSY_REASON_UNSPECIFIED {
			return "", false
		}
		return observation.BusyUnknown, true
	}
	switch reason {
	case platformv1.BusyReason_BUSY_REASON_ON_CALL:
		return observation.BusyOnCall, true
	case platformv1.BusyReason_BUSY_REASON_FOCUSED:
		return observation.BusyFocused, true
	default:
		return "", false
	}
}
