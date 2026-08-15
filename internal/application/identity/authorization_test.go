package identity

import (
	"reflect"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestResolveAuthorizedAtRequiresPermissionsForEveryPresentFragment(t *testing.T) {
	now := testNow()
	tests := []struct {
		name        string
		evidence    Evidence
		permissions []privacy.Permission
	}{
		{name: "face detection", evidence: Evidence{FaceDetection: faceDetection(now, 0)}, permissions: []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection}},
		{name: "empty face identification", evidence: Evidence{FaceIdentification: faceIdentification(now)}, permissions: []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification}},
		{name: "face liveness", evidence: Evidence{FaceLiveness: faceLiveness(now, LivenessUnknown)}, permissions: []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceLiveness}},
		{name: "empty speaker identification", evidence: Evidence{SpeakerIdentification: speakerIdentification(now)}, permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification}},
		{name: "empty speaker verification", evidence: Evidence{SpeakerVerification: &SpeakerVerificationEvidence{OccurredAt: now, ExpectedProfileRef: "profile-a"}}, permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerVerification}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, missing := range test.permissions {
				enabled := withoutPermission(test.permissions, missing)
				resolution, err := ResolveAuthorizedAt(testPolicy(), test.evidence, permissionSnapshot(now, enabled...), biometric.Snapshot{}, now)
				if err != nil {
					t.Fatalf("ResolveAuthorizedAt(missing %s) error = %v", missing, err)
				}
				assertAnonymousReason(t, resolution, ReasonBiometricPermissionMissing)
			}
			resolution, err := ResolveAuthorizedAt(testPolicy(), test.evidence, permissionSnapshot(now, test.permissions...), biometric.Snapshot{}, now)
			if err != nil {
				t.Fatalf("ResolveAuthorizedAt(all permissions) error = %v", err)
			}
			assertAnonymousReason(t, resolution, ReasonNoMatch)
		})
	}
}

func TestResolveAuthorizedAtRequiresActiveEnrollmentForEveryCandidate(t *testing.T) {
	now := testNow()
	tests := []struct {
		name        string
		evidence    Evidence
		permissions []privacy.Permission
		capability  readiness.CapabilityKind
		model       string
		assurance   Assurance
		reason      Reason
	}{
		{
			name: "face identification",
			evidence: Evidence{
				FaceDetection:      faceDetection(now, 1),
				FaceIdentification: faceIdentification(now, faceCandidate("face-1", "profile-a", 1)),
				FaceLiveness:       faceLiveness(now, LivenessPassed),
			},
			permissions: []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness},
			capability:  readiness.FaceIdentification, model: "face-model.v1", assurance: Recognized, reason: ReasonFaceIdentified,
		},
		{
			name:        "speaker identification",
			evidence:    Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1))},
			permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification},
			capability:  readiness.SpeakerIdentification, model: "speaker-model.v1", assurance: Recognized, reason: ReasonSpeakerIdentified,
		},
		{
			name: "speaker verification",
			evidence: Evidence{SpeakerVerification: &SpeakerVerificationEvidence{
				OccurredAt: now, ExpectedProfileRef: "profile-a", Candidates: []SpeakerVerificationCandidate{verificationCandidate("verification-1", "profile-a", 1)},
			}},
			permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerVerification},
			capability:  readiness.SpeakerVerification, model: "speaker-verification-model.v1", assurance: Verified, reason: ReasonSpeakerVerified,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAuthorizedAt(
				testPolicy(), test.evidence, permissionSnapshot(now, test.permissions...),
				biometric.Snapshot{Revision: 1, Records: []biometric.Record{activeEnrollment("profile-a", test.capability, test.model, now)}}, now,
			)
			if err != nil {
				t.Fatalf("ResolveAuthorizedAt() error = %v", err)
			}
			if resolution.Assurance != test.assurance || resolution.ProfileRef != "profile-a" || resolution.Reason != test.reason {
				t.Fatalf("resolution = %#v", resolution)
			}
		})
	}
}

func TestResolveAuthorizedAtFailsClosedForEnrollmentAndModelMismatch(t *testing.T) {
	now := testNow()
	permissions := permissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerIdentification)
	evidence := Evidence{SpeakerIdentification: speakerIdentification(now, speakerCandidate("speaker-1", "profile-a", 1))}
	tests := []struct {
		name    string
		records []biometric.Record
		reason  Reason
	}{
		{name: "missing enrollment", reason: ReasonEnrollmentUnavailable},
		{name: "consent revoked", records: []biometric.Record{func() biometric.Record {
			record := activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", now)
			record.Consented = false
			return record
		}()}, reason: ReasonEnrollmentUnavailable},
		{name: "deletion pending", records: []biometric.Record{func() biometric.Record {
			record := activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", now)
			record.Status = biometric.EnrollmentDeletePending
			return record
		}()}, reason: ReasonEnrollmentUnavailable},
		{name: "model version mismatch", records: []biometric.Record{activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v2", now)}, reason: ReasonModelVersionMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAuthorizedAt(testPolicy(), evidence, permissions, biometric.Snapshot{Revision: 1, Records: test.records}, now)
			if err != nil {
				t.Fatalf("ResolveAuthorizedAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, test.reason)
		})
	}
}

func TestResolveAuthorizedAtNeverPartiallyAcceptsUnauthorizedCandidates(t *testing.T) {
	now := testNow()
	evidence := Evidence{SpeakerIdentification: speakerIdentification(now,
		speakerCandidate("speaker-1", "profile-a", 1),
		speakerCandidate("speaker-2", "profile-b", 0.1),
	)}
	records := []biometric.Record{activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", now)}
	resolution, err := ResolveAuthorizedAt(
		testPolicy(), evidence,
		permissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerIdentification),
		biometric.Snapshot{Revision: 1, Records: records}, now,
	)
	if err != nil {
		t.Fatalf("ResolveAuthorizedAt() error = %v", err)
	}
	assertAnonymousReason(t, resolution, ReasonEnrollmentUnavailable)

	reversed := evidence
	reversed.SpeakerIdentification = speakerIdentification(now,
		evidence.SpeakerIdentification.Candidates[1], evidence.SpeakerIdentification.Candidates[0],
	)
	again, err := ResolveAuthorizedAt(
		testPolicy(), reversed,
		permissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerIdentification),
		biometric.Snapshot{Revision: 1, Records: records}, now,
	)
	if err != nil || !reflect.DeepEqual(again, resolution) {
		t.Fatalf("reordered resolution = %#v, %v, want %#v", again, err, resolution)
	}
}

func TestResolveAuthorizedAtRejectsMalformedPolicyEvidenceAndSnapshots(t *testing.T) {
	now := testNow()
	validPermissions := permissionSnapshot(now, privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness)
	validCatalog := biometric.Snapshot{Revision: 1, Records: []biometric.Record{activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now)}}
	tests := []struct {
		name        string
		policy      Policy
		evidence    Evidence
		permissions privacy.Snapshot
		catalog     biometric.Snapshot
	}{
		{name: "invalid evidence", policy: testPolicy(), evidence: Evidence{SpeakerIdentification: &SpeakerIdentificationEvidence{}}, permissions: validPermissions, catalog: validCatalog},
		{name: "duplicate permission", policy: testPolicy(), evidence: validFaceEvidence(now), permissions: privacy.Snapshot{Revision: 1, Grants: []privacy.Grant{{Permission: privacy.FaceIdentification, Enabled: true, UpdatedAt: now}, {Permission: privacy.FaceIdentification, Enabled: true, UpdatedAt: now}}}, catalog: validCatalog},
		{name: "duplicate enrollment", policy: testPolicy(), evidence: validFaceEvidence(now), permissions: validPermissions, catalog: biometric.Snapshot{Revision: 1, Records: []biometric.Record{activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now), activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveAuthorizedAt(test.policy, test.evidence, test.permissions, test.catalog, now)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("ResolveAuthorizedAt() error = %v, want InvalidInput", err)
			}
		})
	}
}

func withoutPermission(permissions []privacy.Permission, missing privacy.Permission) []privacy.Permission {
	enabled := make([]privacy.Permission, 0, len(permissions)-1)
	for _, permission := range permissions {
		if permission != missing {
			enabled = append(enabled, permission)
		}
	}
	return enabled
}

func assertAnonymousReason(t *testing.T, resolution Resolution, reason Reason) {
	t.Helper()
	if resolution.Assurance != Anonymous || resolution.ProfileRef != "" || resolution.Reason != reason || resolution.PolicyVersion != testPolicy().Version {
		t.Fatalf("resolution = %#v, want ANONYMOUS/%s", resolution, reason)
	}
}

func activeEnrollment(profile string, capability readiness.CapabilityKind, modelVersion string, at time.Time) biometric.Record {
	return biometric.Record{
		ProfileRef: profile, Capability: capability,
		Consented: true, ConsentVersion: 1, ConsentUpdatedAt: at,
		TemplateRef: "template-" + profile + "-" + string(capability), ModelVersion: modelVersion,
		Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
	}
}

func permissionSnapshot(at time.Time, enabled ...privacy.Permission) privacy.Snapshot {
	selected := make(map[privacy.Permission]struct{}, len(enabled))
	for _, permission := range enabled {
		selected[permission] = struct{}{}
	}
	snapshot := privacy.Snapshot{Revision: 1, Grants: make([]privacy.Grant, 0, len(privacy.AllPermissions()))}
	for _, permission := range privacy.AllPermissions() {
		_, active := selected[permission]
		grant := privacy.Grant{Permission: permission, Enabled: active}
		if active {
			grant.UpdatedAt = at
		}
		snapshot.Grants = append(snapshot.Grants, grant)
	}
	return snapshot
}
