package identity

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

const resolveOp = "resolve identity evidence"

// ResolveAt validates and resolves one immutable evidence set at the supplied
// evaluation time. It performs no I/O and reads no ambient clock.
func ResolveAt(policy Policy, evidence Evidence, now time.Time) (Resolution, error) {
	times, err := validateAt(policy, evidence, now)
	if err != nil {
		return Resolution{}, err
	}
	if expired(times, now, policy.MaxEvidenceAge) {
		return anonymous(policy, ReasonEvidenceExpired), nil
	}
	if outsideSkew(times, policy.MaxEvidenceSkew) {
		return anonymous(policy, ReasonEvidenceTimeSkew), nil
	}
	if len(evidence.SpeakerVerifications) > 0 {
		return resolveVerification(policy, evidence), nil
	}
	return resolveIdentification(policy, evidence), nil
}

// ValidateAt checks policy and candidate shape without resolving a profile.
// Privacy authorization is a separate gate applied by ResolveAuthorizedAt.
func ValidateAt(policy Policy, evidence Evidence, now time.Time) error {
	_, err := validateAt(policy, evidence, now)
	return err
}

func validateAt(policy Policy, evidence Evidence, now time.Time) ([]time.Time, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	times, err := validateEvidence(evidence, now)
	if err != nil {
		return nil, err
	}
	return times, nil
}

func validatePolicy(policy Policy) error {
	if !validRequiredString(policy.Version) {
		return invalidInput("policy version is required")
	}
	thresholds := []struct {
		name  string
		value float64
	}{
		{"face identification", policy.FaceIdentificationThreshold},
		{"speaker identification", policy.SpeakerIdentificationThreshold},
		{"speaker verification", policy.SpeakerVerificationThreshold},
	}
	for _, threshold := range thresholds {
		if !validThreshold(threshold.value) {
			return invalidInput("%s threshold must be within (0,1]", threshold.name)
		}
	}
	if policy.MaxEvidenceAge <= 0 || policy.MaxEvidenceSkew <= 0 {
		return invalidInput("evidence age and skew bounds must be positive")
	}
	return nil
}

func validateEvidence(evidence Evidence, now time.Time) ([]time.Time, error) {
	if now.IsZero() {
		return nil, invalidInput("evaluation time is required")
	}
	identifications := len(evidence.FaceIdentifications) + len(evidence.SpeakerIdentifications)
	verifications := len(evidence.SpeakerVerifications)
	if identifications+verifications == 0 {
		return nil, invalidInput("at least one identity candidate is required")
	}
	if len(evidence.FaceIdentifications) == 0 && evidence.FacesObserved != 0 {
		return nil, invalidInput("face count requires face identification evidence")
	}
	if len(evidence.FaceIdentifications) > 0 && evidence.FacesObserved == 0 {
		return nil, invalidInput("face identification evidence requires a positive face count")
	}
	if verifications > 0 {
		if identifications > 0 || evidence.FacesObserved != 0 {
			return nil, invalidInput("identification and verification evidence must not be mixed")
		}
		if !validRequiredString(evidence.ExpectedProfileRef) {
			return nil, invalidInput("speaker verification requires an expected profile reference")
		}
	} else if evidence.ExpectedProfileRef != "" {
		return nil, invalidInput("expected profile reference is valid only for speaker verification")
	}

	seenIDs := make(map[string]struct{}, identifications+verifications)
	times := make([]time.Time, 0, identifications+verifications)
	for _, candidate := range evidence.FaceIdentifications {
		if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, candidate.OccurredAt, now, seenIDs); err != nil {
			return nil, err
		}
		if candidate.Liveness != LivenessUnknown && candidate.Liveness != LivenessPassed && candidate.Liveness != LivenessFailed {
			return nil, invalidInput("face candidate %q has unknown liveness", candidate.ID)
		}
		times = append(times, candidate.OccurredAt)
	}
	for _, candidate := range evidence.SpeakerIdentifications {
		if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, candidate.OccurredAt, now, seenIDs); err != nil {
			return nil, err
		}
		times = append(times, candidate.OccurredAt)
	}
	for _, candidate := range evidence.SpeakerVerifications {
		if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, candidate.OccurredAt, now, seenIDs); err != nil {
			return nil, err
		}
		times = append(times, candidate.OccurredAt)
	}
	return times, nil
}

func validateCandidate(id, profileRef string, score float64, modelVersion string, occurredAt, now time.Time, seenIDs map[string]struct{}) error {
	if !validRequiredString(id) || !validRequiredString(profileRef) || !validRequiredString(modelVersion) {
		return invalidInput("candidate identity, profile reference, and model version are required")
	}
	if _, exists := seenIDs[id]; exists {
		return invalidInput("candidate id %q is duplicated", id)
	}
	seenIDs[id] = struct{}{}
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
		return invalidInput("candidate %q score is outside [0,1]", id)
	}
	if occurredAt.IsZero() {
		return invalidInput("candidate %q event time is required", id)
	}
	if occurredAt.After(now) {
		return invalidInput("candidate %q event time is in the future", id)
	}
	return nil
}

func resolveIdentification(policy Policy, evidence Evidence) Resolution {
	faceProfiles, faceCandidates := qualifyingFaces(evidence.FaceIdentifications, policy.FaceIdentificationThreshold)
	if len(faceProfiles) > 1 {
		return anonymous(policy, ReasonMultipleFaceMatches)
	}
	if evidence.FacesObserved > 1 {
		return anonymous(policy, ReasonMultiplePeople)
	}
	if len(faceProfiles) == 1 && policy.RequireFaceLiveness {
		failed, missing := livenessState(faceCandidates)
		if failed {
			return anonymous(policy, ReasonLivenessFailed)
		}
		if missing {
			return anonymous(policy, ReasonLivenessMissing)
		}
	}

	speakerProfiles := qualifyingSpeakerProfiles(evidence.SpeakerIdentifications, policy.SpeakerIdentificationThreshold)
	if len(speakerProfiles) > 1 {
		return anonymous(policy, ReasonMultipleSpeakerMatches)
	}
	if len(faceProfiles) == 1 && len(speakerProfiles) == 1 {
		if faceProfiles[0] != speakerProfiles[0] {
			return anonymous(policy, ReasonModalityConflict)
		}
		return recognized(policy, faceProfiles[0], ReasonModalitiesMatched)
	}
	if len(faceProfiles) == 1 {
		return recognized(policy, faceProfiles[0], ReasonFaceIdentified)
	}
	if len(speakerProfiles) == 1 {
		return recognized(policy, speakerProfiles[0], ReasonSpeakerIdentified)
	}
	return anonymous(policy, ReasonNoMatch)
}

func resolveVerification(policy Policy, evidence Evidence) Resolution {
	profiles := make(map[string]struct{})
	for _, candidate := range evidence.SpeakerVerifications {
		if candidate.Score >= policy.SpeakerVerificationThreshold {
			profiles[candidate.ProfileRef] = struct{}{}
		}
	}
	qualified := sortedProfiles(profiles)
	if len(qualified) > 1 {
		return anonymous(policy, ReasonMultipleVerificationMatches)
	}
	if len(qualified) == 0 {
		return anonymous(policy, ReasonNoMatch)
	}
	if qualified[0] != evidence.ExpectedProfileRef {
		return anonymous(policy, ReasonVerificationTargetMismatch)
	}
	return Resolution{
		Assurance: Verified, ProfileRef: qualified[0], Reason: ReasonSpeakerVerified, PolicyVersion: policy.Version,
	}
}

func qualifyingFaces(candidates []FaceIdentificationCandidate, threshold float64) ([]string, []FaceIdentificationCandidate) {
	profiles := make(map[string]struct{})
	qualified := make([]FaceIdentificationCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score < threshold {
			continue
		}
		profiles[candidate.ProfileRef] = struct{}{}
		qualified = append(qualified, candidate)
	}
	return sortedProfiles(profiles), qualified
}

func qualifyingSpeakerProfiles(candidates []SpeakerIdentificationCandidate, threshold float64) []string {
	profiles := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.Score >= threshold {
			profiles[candidate.ProfileRef] = struct{}{}
		}
	}
	return sortedProfiles(profiles)
}

func livenessState(candidates []FaceIdentificationCandidate) (failed bool, missing bool) {
	for _, candidate := range candidates {
		switch candidate.Liveness {
		case LivenessFailed:
			failed = true
		case LivenessUnknown:
			missing = true
		}
	}
	return failed, missing
}

func sortedProfiles(profiles map[string]struct{}) []string {
	output := make([]string, 0, len(profiles))
	for profile := range profiles {
		output = append(output, profile)
	}
	sort.Strings(output)
	return output
}

func expired(times []time.Time, now time.Time, maximumAge time.Duration) bool {
	for _, occurredAt := range times {
		if now.Sub(occurredAt) > maximumAge {
			return true
		}
	}
	return false
}

func outsideSkew(times []time.Time, maximumSkew time.Duration) bool {
	oldest, newest := times[0], times[0]
	for _, occurredAt := range times[1:] {
		if occurredAt.Before(oldest) {
			oldest = occurredAt
		}
		if occurredAt.After(newest) {
			newest = occurredAt
		}
	}
	return newest.Sub(oldest) > maximumSkew
}

func anonymous(policy Policy, reason Reason) Resolution {
	return Resolution{Assurance: Anonymous, Reason: reason, PolicyVersion: policy.Version}
}

func recognized(policy Policy, profileRef string, reason Reason) Resolution {
	return Resolution{Assurance: Recognized, ProfileRef: profileRef, Reason: reason, PolicyVersion: policy.Version}
}

func validThreshold(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && value <= 1
}

func validRequiredString(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func invalidInput(format string, args ...any) error {
	return fault.New(fault.InvalidInput, resolveOp, fmt.Errorf(format, args...))
}
