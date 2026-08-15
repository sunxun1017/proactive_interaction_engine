package ingress

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/readiness"
)

const (
	maxCandidates         = 16
	maxEvidenceTTL        = 10 * time.Second
	maxIdentifierLength   = 128
	maxModelVersionLength = 64
	maxTraceIDLength      = 256
)

type evidenceMetadata struct {
	fragmentID       string
	evidenceWindowID string
	providerLeaseID  string
	sourceInstanceID string
	sourceSeq        uint64
	occurredAt       time.Time
	expiresAt        time.Time
	traceID          string
}

type identificationFragment struct {
	metadata             evidenceMetadata
	capability           readiness.CapabilityKind
	requiredCapabilities []readiness.CapabilityKind
	evidence             identity.Evidence
}

type verificationFragment struct {
	metadata             evidenceMetadata
	capability           readiness.CapabilityKind
	requiredCapabilities []readiness.CapabilityKind
	challengeID          string
	candidate            identity.SpeakerVerificationCandidate
}

func mapFaceIdentification(request *platformv1.PublishFaceIdentificationEvidenceRequest, now time.Time) (identificationFragment, error) {
	if request == nil {
		return identificationFragment{}, errors.New("face identification request is required")
	}
	metadata, err := mapEvidenceMetadata(request.GetMetadata(), now)
	if err != nil {
		return identificationFragment{}, err
	}
	if request.GetFacesObserved() == 0 {
		return identificationFragment{}, errors.New("faces observed must be positive")
	}
	if err := validateCandidateCount(len(request.GetCandidates())); err != nil {
		return identificationFragment{}, err
	}

	candidates := make([]identity.FaceIdentificationCandidate, 0, len(request.GetCandidates()))
	seen := make(map[string]struct{}, len(request.GetCandidates()))
	requiresLiveness := false
	modelVersion := ""
	for _, wire := range request.GetCandidates() {
		if wire == nil {
			return identificationFragment{}, errors.New("face candidate is required")
		}
		if err := validateCandidate(wire.GetCandidateId(), wire.GetProfileRef(), wire.GetModelVersion(), wire.GetScore(), seen); err != nil {
			return identificationFragment{}, err
		}
		if modelVersion != "" && wire.GetModelVersion() != modelVersion {
			return identificationFragment{}, errors.New("face candidates use mixed model versions")
		}
		modelVersion = wire.GetModelVersion()
		liveness, ok := mapLiveness(wire.GetLiveness())
		if !ok {
			return identificationFragment{}, errors.New("face liveness is invalid")
		}
		if liveness != identity.LivenessUnknown {
			requiresLiveness = true
		}
		candidates = append(candidates, identity.FaceIdentificationCandidate{
			ID:           wire.GetCandidateId(),
			ProfileRef:   wire.GetProfileRef(),
			Score:        wire.GetScore(),
			ModelVersion: wire.GetModelVersion(),
			OccurredAt:   metadata.occurredAt,
			Liveness:     liveness,
		})
	}

	required := []readiness.CapabilityKind{readiness.FaceIdentification, readiness.FaceDetection}
	if requiresLiveness {
		required = append(required, readiness.FaceLiveness)
	}
	return identificationFragment{
		metadata:             metadata,
		capability:           readiness.FaceIdentification,
		requiredCapabilities: required,
		evidence: identity.Evidence{
			FacesObserved:       request.GetFacesObserved(),
			FaceIdentifications: candidates,
		},
	}, nil
}

func mapSpeakerIdentification(request *platformv1.PublishSpeakerIdentificationEvidenceRequest, now time.Time) (identificationFragment, error) {
	if request == nil {
		return identificationFragment{}, errors.New("speaker identification request is required")
	}
	metadata, err := mapEvidenceMetadata(request.GetMetadata(), now)
	if err != nil {
		return identificationFragment{}, err
	}
	if err := validateCandidateCount(len(request.GetCandidates())); err != nil {
		return identificationFragment{}, err
	}

	candidates := make([]identity.SpeakerIdentificationCandidate, 0, len(request.GetCandidates()))
	seen := make(map[string]struct{}, len(request.GetCandidates()))
	modelVersion := ""
	for _, wire := range request.GetCandidates() {
		if wire == nil {
			return identificationFragment{}, errors.New("speaker candidate is required")
		}
		if err := validateCandidate(wire.GetCandidateId(), wire.GetProfileRef(), wire.GetModelVersion(), wire.GetScore(), seen); err != nil {
			return identificationFragment{}, err
		}
		if modelVersion != "" && wire.GetModelVersion() != modelVersion {
			return identificationFragment{}, errors.New("speaker candidates use mixed model versions")
		}
		modelVersion = wire.GetModelVersion()
		candidates = append(candidates, identity.SpeakerIdentificationCandidate{
			ID:           wire.GetCandidateId(),
			ProfileRef:   wire.GetProfileRef(),
			Score:        wire.GetScore(),
			ModelVersion: wire.GetModelVersion(),
			OccurredAt:   metadata.occurredAt,
		})
	}

	return identificationFragment{
		metadata:             metadata,
		capability:           readiness.SpeakerIdentification,
		requiredCapabilities: []readiness.CapabilityKind{readiness.SpeakerIdentification},
		evidence:             identity.Evidence{SpeakerIdentifications: candidates},
	}, nil
}

func mapSpeakerVerification(request *platformv1.PublishSpeakerVerificationEvidenceRequest, now time.Time) (verificationFragment, error) {
	if request == nil {
		return verificationFragment{}, errors.New("speaker verification request is required")
	}
	metadata, err := mapEvidenceMetadata(request.GetMetadata(), now)
	if err != nil {
		return verificationFragment{}, err
	}
	if !validIdentifier(request.GetVerificationChallengeId()) {
		return verificationFragment{}, errors.New("verification challenge id is invalid")
	}
	if !validIdentifier(request.GetCandidateId()) || !validModelVersion(request.GetModelVersion()) || !validScore(request.GetScore()) {
		return verificationFragment{}, errors.New("speaker verification candidate is invalid")
	}

	return verificationFragment{
		metadata:             metadata,
		capability:           readiness.SpeakerVerification,
		requiredCapabilities: []readiness.CapabilityKind{readiness.SpeakerVerification},
		challengeID:          request.GetVerificationChallengeId(),
		candidate: identity.SpeakerVerificationCandidate{
			ID:           request.GetCandidateId(),
			Score:        request.GetScore(),
			ModelVersion: request.GetModelVersion(),
			OccurredAt:   metadata.occurredAt,
		},
	}, nil
}

func mapEvidenceMetadata(wire *platformv1.IdentityEvidenceMetadata, now time.Time) (evidenceMetadata, error) {
	if wire == nil {
		return evidenceMetadata{}, errors.New("identity evidence metadata is required")
	}
	if !validIdentifier(wire.GetFragmentId()) || !validIdentifier(wire.GetEvidenceWindowId()) || !validIdentifier(wire.GetProviderLeaseId()) || !validIdentifier(wire.GetSourceInstanceId()) || !validBoundedString(wire.GetTraceId(), maxTraceIDLength) {
		return evidenceMetadata{}, errors.New("identity evidence metadata contains an invalid identifier")
	}
	if wire.GetSourceSeq() == 0 {
		return evidenceMetadata{}, errors.New("identity evidence source sequence must be positive")
	}
	if wire.GetOccurredAt() == nil || wire.GetOccurredAt().CheckValid() != nil {
		return evidenceMetadata{}, errors.New("identity evidence timestamp is invalid")
	}
	if wire.GetTtl() == nil || wire.GetTtl().CheckValid() != nil || wire.GetTtl().AsDuration() <= 0 || wire.GetTtl().AsDuration() > maxEvidenceTTL {
		return evidenceMetadata{}, errors.New("identity evidence ttl is invalid")
	}
	occurredAt := wire.GetOccurredAt().AsTime()
	if occurredAt.After(now) {
		return evidenceMetadata{}, errors.New("identity evidence timestamp is in the future")
	}
	expiresAt := occurredAt.Add(wire.GetTtl().AsDuration())
	if !expiresAt.After(now) {
		return evidenceMetadata{}, errors.New("identity evidence is stale")
	}
	return evidenceMetadata{
		fragmentID:       wire.GetFragmentId(),
		evidenceWindowID: wire.GetEvidenceWindowId(),
		providerLeaseID:  wire.GetProviderLeaseId(),
		sourceInstanceID: wire.GetSourceInstanceId(),
		sourceSeq:        wire.GetSourceSeq(),
		occurredAt:       occurredAt,
		expiresAt:        expiresAt,
		traceID:          wire.GetTraceId(),
	}, nil
}

func validateCandidateCount(count int) error {
	if count == 0 || count > maxCandidates {
		return fmt.Errorf("candidate count must be between 1 and %d", maxCandidates)
	}
	return nil
}

func validateCandidate(id, profileRef, modelVersion string, score float64, seen map[string]struct{}) error {
	if !validIdentifier(id) || !validIdentifier(profileRef) || !validModelVersion(modelVersion) || !validScore(score) {
		return errors.New("identity candidate is invalid")
	}
	if _, exists := seen[id]; exists {
		return errors.New("identity candidate id is duplicated")
	}
	seen[id] = struct{}{}
	return nil
}

func validIdentifier(value string) bool {
	return validBoundedString(value, maxIdentifierLength)
}

func validModelVersion(value string) bool {
	return validBoundedString(value, maxModelVersionLength)
}

func validBoundedString(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0) && score >= 0 && score <= 1
}

func mapLiveness(value platformv1.FaceLivenessState) (identity.Liveness, bool) {
	switch value {
	case platformv1.FaceLivenessState_FACE_LIVENESS_STATE_UNKNOWN:
		return identity.LivenessUnknown, true
	case platformv1.FaceLivenessState_FACE_LIVENESS_STATE_PASSED:
		return identity.LivenessPassed, true
	case platformv1.FaceLivenessState_FACE_LIVENESS_STATE_FAILED:
		return identity.LivenessFailed, true
	default:
		return "", false
	}
}
