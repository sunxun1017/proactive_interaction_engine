package identity

import (
	"fmt"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

const authorizeOp = "authorize identity evidence"

// ResolveAuthorizedAt applies the privacy and enrollment gate before identity
// resolution. Any unauthorized candidate makes the complete evidence set
// anonymous; it is never partially filtered into a successful identity.
func ResolveAuthorizedAt(
	policy Policy,
	evidence Evidence,
	permissions privacy.Snapshot,
	enrollments biometric.Snapshot,
	now time.Time,
) (Resolution, error) {
	if err := ValidateAt(policy, evidence, now); err != nil {
		return Resolution{}, err
	}
	enabled, err := enabledPermissions(permissions)
	if err != nil {
		return Resolution{}, err
	}
	for _, required := range requiredPermissions(policy, evidence) {
		if _, ok := enabled[required]; !ok {
			return authorizationAnonymous(policy, ReasonBiometricPermissionMissing), nil
		}
	}
	if err := biometric.ValidateSnapshot(enrollments); err != nil {
		return Resolution{}, err
	}

	index := enrollmentIndex(enrollments)
	enrollmentUnavailable := false
	modelVersionMismatch := false
	check := func(profileRef string, capability readiness.CapabilityKind, modelVersion string) {
		record, exists := index[enrollmentKey{profileRef: profileRef, capability: capability}]
		if !exists || !record.Consented || record.Status != biometric.EnrollmentActive {
			enrollmentUnavailable = true
			return
		}
		if record.ModelVersion != modelVersion {
			modelVersionMismatch = true
		}
	}
	for _, candidate := range evidence.FaceIdentifications {
		check(candidate.ProfileRef, readiness.FaceIdentification, candidate.ModelVersion)
	}
	for _, candidate := range evidence.SpeakerIdentifications {
		check(candidate.ProfileRef, readiness.SpeakerIdentification, candidate.ModelVersion)
	}
	for _, candidate := range evidence.SpeakerVerifications {
		check(candidate.ProfileRef, readiness.SpeakerVerification, candidate.ModelVersion)
	}
	if enrollmentUnavailable {
		return authorizationAnonymous(policy, ReasonEnrollmentUnavailable), nil
	}
	if modelVersionMismatch {
		return authorizationAnonymous(policy, ReasonModelVersionMismatch), nil
	}
	return ResolveAt(policy, evidence, now)
}

func enabledPermissions(snapshot privacy.Snapshot) (map[privacy.Permission]struct{}, error) {
	known := make(map[privacy.Permission]struct{}, len(privacy.AllPermissions()))
	for _, permission := range privacy.AllPermissions() {
		known[permission] = struct{}{}
	}
	seen := make(map[privacy.Permission]struct{}, len(snapshot.Grants))
	enabled := make(map[privacy.Permission]struct{})
	for _, grant := range snapshot.Grants {
		if _, ok := known[grant.Permission]; !ok {
			return nil, authorizationInvalid("unknown permission %q", grant.Permission)
		}
		if _, duplicate := seen[grant.Permission]; duplicate {
			return nil, authorizationInvalid("permission %q is duplicated", grant.Permission)
		}
		seen[grant.Permission] = struct{}{}
		if snapshot.Revision == 0 && (grant.Enabled || !grant.UpdatedAt.IsZero()) {
			return nil, authorizationInvalid("revision zero contains changed permission %q", grant.Permission)
		}
		if grant.Enabled {
			if grant.UpdatedAt.IsZero() {
				return nil, authorizationInvalid("enabled permission %q has no update time", grant.Permission)
			}
			enabled[grant.Permission] = struct{}{}
		}
	}
	return enabled, nil
}

func requiredPermissions(policy Policy, evidence Evidence) []privacy.Permission {
	required := make(map[privacy.Permission]struct{})
	if len(evidence.FaceIdentifications) > 0 {
		required[privacy.CameraCapture] = struct{}{}
		required[privacy.FaceDetection] = struct{}{}
		required[privacy.FaceIdentification] = struct{}{}
		usesLiveness := policy.RequireFaceLiveness
		for _, candidate := range evidence.FaceIdentifications {
			usesLiveness = usesLiveness || candidate.Liveness != LivenessUnknown
		}
		if usesLiveness {
			required[privacy.FaceLiveness] = struct{}{}
		}
	}
	if len(evidence.SpeakerIdentifications) > 0 {
		required[privacy.MicrophoneCapture] = struct{}{}
		required[privacy.SpeakerIdentification] = struct{}{}
	}
	if len(evidence.SpeakerVerifications) > 0 {
		required[privacy.MicrophoneCapture] = struct{}{}
		required[privacy.SpeakerVerification] = struct{}{}
	}
	ordered := make([]privacy.Permission, 0, len(required))
	for _, permission := range privacy.AllPermissions() {
		if _, ok := required[permission]; ok {
			ordered = append(ordered, permission)
		}
	}
	return ordered
}

type enrollmentKey struct {
	profileRef string
	capability readiness.CapabilityKind
}

func enrollmentIndex(snapshot biometric.Snapshot) map[enrollmentKey]biometric.Record {
	indexed := make(map[enrollmentKey]biometric.Record, len(snapshot.Records))
	for _, record := range snapshot.Records {
		indexed[enrollmentKey{profileRef: record.ProfileRef, capability: record.Capability}] = record
	}
	return indexed
}

func authorizationAnonymous(policy Policy, reason Reason) Resolution {
	return Resolution{Assurance: Anonymous, Reason: reason, PolicyVersion: policy.Version}
}

func authorizationInvalid(format string, args ...any) error {
	return fault.New(fault.InvalidInput, authorizeOp, fmt.Errorf(format, args...))
}
