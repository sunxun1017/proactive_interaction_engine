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
	if evidence.FaceDetection != nil && evidence.FaceDetection.FacesObserved > 1 {
		return anonymous(policy, ReasonMultiplePeople), nil
	}
	if evidence.SpeakerVerification != nil {
		return resolveVerification(policy, *evidence.SpeakerVerification), nil
	}
	return resolveIdentification(policy, evidence), nil
}

// ValidateAt checks policy and evidence shape without resolving a profile.
// Privacy authorization is a separate gate applied by ResolveAuthorizedAt.
func ValidateAt(policy Policy, evidence Evidence, now time.Time) error {
	_, err := validateAt(policy, evidence, now)
	return err
}

func validateAt(policy Policy, evidence Evidence, now time.Time) ([]time.Time, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	return validateEvidence(evidence, now)
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
	if evidence.SpeakerVerification != nil &&
		(evidence.FaceIdentification != nil || evidence.SpeakerIdentification != nil) {
		return nil, invalidInput("identification and verification evidence must not be mixed")
	}

	times := make([]time.Time, 0, 5)
	appendTime := func(kind string, occurredAt time.Time) error {
		if occurredAt.IsZero() {
			return invalidInput("%s evidence time is required", kind)
		}
		if occurredAt.After(now) {
			return invalidInput("%s evidence time is in the future", kind)
		}
		times = append(times, occurredAt)
		return nil
	}

	if fragment := evidence.FaceDetection; fragment != nil {
		if err := appendTime("face detection", fragment.OccurredAt); err != nil {
			return nil, err
		}
	}
	if fragment := evidence.FaceIdentification; fragment != nil {
		if err := appendTime("face identification", fragment.OccurredAt); err != nil {
			return nil, err
		}
		seenIDs := make(map[string]struct{}, len(fragment.Candidates))
		for _, candidate := range fragment.Candidates {
			if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, seenIDs); err != nil {
				return nil, err
			}
		}
	}
	if fragment := evidence.FaceLiveness; fragment != nil {
		if err := appendTime("face liveness", fragment.OccurredAt); err != nil {
			return nil, err
		}
		if fragment.State != LivenessUnknown && fragment.State != LivenessPassed && fragment.State != LivenessFailed {
			return nil, invalidInput("face liveness state is unknown")
		}
	}
	if fragment := evidence.SpeakerIdentification; fragment != nil {
		if err := appendTime("speaker identification", fragment.OccurredAt); err != nil {
			return nil, err
		}
		seenIDs := make(map[string]struct{}, len(fragment.Candidates))
		for _, candidate := range fragment.Candidates {
			if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, seenIDs); err != nil {
				return nil, err
			}
		}
	}
	if fragment := evidence.SpeakerVerification; fragment != nil {
		if err := appendTime("speaker verification", fragment.OccurredAt); err != nil {
			return nil, err
		}
		if !validRequiredString(fragment.ExpectedProfileRef) {
			return nil, invalidInput("speaker verification requires an expected profile reference")
		}
		seenIDs := make(map[string]struct{}, len(fragment.Candidates))
		for _, candidate := range fragment.Candidates {
			if err := validateCandidate(candidate.ID, candidate.ProfileRef, candidate.Score, candidate.ModelVersion, seenIDs); err != nil {
				return nil, err
			}
		}
	}
	return times, nil
}

func validateCandidate(id, profileRef string, score float64, modelVersion string, seenIDs map[string]struct{}) error {
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
	return nil
}

func resolveIdentification(policy Policy, evidence Evidence) Resolution {
	if evidence.FaceDetection != nil && evidence.FaceDetection.FacesObserved == 0 &&
		evidence.FaceIdentification != nil && len(evidence.FaceIdentification.Candidates) > 0 {
		return anonymous(policy, ReasonFaceDetectionMismatch)
	}

	faceProfiles := []string(nil)
	if evidence.FaceDetection != nil && evidence.FaceDetection.FacesObserved == 1 && evidence.FaceIdentification != nil {
		faceProfiles = qualifyingFaceProfiles(evidence.FaceIdentification.Candidates, policy.FaceIdentificationThreshold)
	}
	if len(faceProfiles) > 1 {
		return anonymous(policy, ReasonMultipleFaceMatches)
	}

	speakerProfiles := []string(nil)
	if evidence.SpeakerIdentification != nil {
		speakerProfiles = qualifyingSpeakerProfiles(evidence.SpeakerIdentification.Candidates, policy.SpeakerIdentificationThreshold)
	}
	if len(speakerProfiles) > 1 {
		return anonymous(policy, ReasonMultipleSpeakerMatches)
	}

	faceAvailable := len(faceProfiles) == 1
	if faceAvailable && policy.RequireFaceLiveness {
		switch {
		case evidence.FaceLiveness == nil || evidence.FaceLiveness.State == LivenessUnknown:
			faceAvailable = false
			if len(speakerProfiles) == 0 {
				return anonymous(policy, ReasonLivenessMissing)
			}
		case evidence.FaceLiveness.State == LivenessFailed:
			faceAvailable = false
			if len(speakerProfiles) == 0 {
				return anonymous(policy, ReasonLivenessFailed)
			}
		}
	}

	if faceAvailable && len(speakerProfiles) == 1 {
		if faceProfiles[0] != speakerProfiles[0] {
			return anonymous(policy, ReasonModalityConflict)
		}
		return recognized(policy, faceProfiles[0], ReasonModalitiesMatched)
	}
	if faceAvailable {
		return recognized(policy, faceProfiles[0], ReasonFaceIdentified)
	}
	if len(speakerProfiles) == 1 {
		return recognized(policy, speakerProfiles[0], ReasonSpeakerIdentified)
	}
	return anonymous(policy, ReasonNoMatch)
}

func resolveVerification(policy Policy, evidence SpeakerVerificationEvidence) Resolution {
	profiles := make(map[string]struct{})
	for _, candidate := range evidence.Candidates {
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

func qualifyingFaceProfiles(candidates []FaceIdentificationCandidate, threshold float64) []string {
	profiles := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.Score >= threshold {
			profiles[candidate.ProfileRef] = struct{}{}
		}
	}
	return sortedProfiles(profiles)
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
	if len(times) < 2 {
		return false
	}
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
