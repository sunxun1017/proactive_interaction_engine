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

func TestResolveAuthorizedAtRequiresGlobalPermissionAndActiveEnrollment(t *testing.T) {
	now := testNow()
	policy := testPolicy()
	tests := []struct {
		name        string
		evidence    Evidence
		permissions []privacy.Permission
		record      biometric.Record
		assurance   Assurance
		reason      Reason
	}{
		{
			name: "face identification",
			evidence: Evidence{FacesObserved: 1, FaceIdentifications: []FaceIdentificationCandidate{
				faceCandidate("face-1", "profile-a", 1, now, LivenessPassed),
			}},
			permissions: []privacy.Permission{
				privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness,
			},
			record: biometric.Record{
				ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
				Consented: true, ConsentUpdatedAt: now, TemplateRef: "face-template",
				ModelVersion: "face-model.v1", Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: now,
			},
			assurance: Recognized, reason: ReasonFaceIdentified,
		},
		{
			name: "speaker identification",
			evidence: Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
				speakerCandidate("speaker-1", "profile-a", 1, now),
			}},
			permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification},
			record: biometric.Record{
				ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification,
				Consented: true, ConsentUpdatedAt: now, TemplateRef: "speaker-template",
				ModelVersion: "speaker-model.v1", Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: now,
			},
			assurance: Recognized, reason: ReasonSpeakerIdentified,
		},
		{
			name: "speaker verification",
			evidence: Evidence{ExpectedProfileRef: "profile-a", SpeakerVerifications: []SpeakerVerificationCandidate{
				verificationCandidate("verification-1", "profile-a", 1, now),
			}},
			permissions: []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerVerification},
			record: biometric.Record{
				ProfileRef: "profile-a", Capability: readiness.SpeakerVerification,
				Consented: true, ConsentUpdatedAt: now, TemplateRef: "verification-template",
				ModelVersion: "speaker-verification-model.v1", Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: now,
			},
			assurance: Verified, reason: ReasonSpeakerVerified,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAuthorizedAt(
				policy, test.evidence, permissionSnapshot(now, test.permissions...),
				biometric.Snapshot{Revision: 1, Records: []biometric.Record{test.record}}, now,
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

func TestResolveAuthorizedAtFailsClosedForEveryMissingFacePermission(t *testing.T) {
	now := testNow()
	required := []privacy.Permission{
		privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness,
	}
	for _, missing := range required {
		t.Run(string(missing), func(t *testing.T) {
			enabled := make([]privacy.Permission, 0, len(required)-1)
			for _, permission := range required {
				if permission != missing {
					enabled = append(enabled, permission)
				}
			}
			resolution, err := ResolveAuthorizedAt(
				testPolicy(), validFaceEvidence(now), permissionSnapshot(now, enabled...),
				biometric.Snapshot{Revision: 1, Records: []biometric.Record{activeEnrollment(
					"profile-a", readiness.FaceIdentification, "face-model.v1", now,
				)}}, now,
			)
			if err != nil {
				t.Fatalf("ResolveAuthorizedAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, ReasonBiometricPermissionMissing)
		})
	}
}

func TestResolveAuthorizedAtFailsClosedForEnrollmentAndModelMismatch(t *testing.T) {
	now := testNow()
	permissions := permissionSnapshot(
		now, privacy.MicrophoneCapture, privacy.SpeakerIdentification,
	)
	evidence := Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
		speakerCandidate("speaker-1", "profile-a", 1, now),
	}}
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
		{name: "model version mismatch", records: []biometric.Record{
			activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v2", now),
		}, reason: ReasonModelVersionMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolution, err := ResolveAuthorizedAt(
				testPolicy(), evidence, permissions,
				biometric.Snapshot{Revision: 1, Records: test.records}, now,
			)
			if err != nil {
				t.Fatalf("ResolveAuthorizedAt() error = %v", err)
			}
			assertAnonymousReason(t, resolution, test.reason)
		})
	}
}

func TestResolveAuthorizedAtNeverPartiallyAcceptsUnauthorizedCandidates(t *testing.T) {
	now := testNow()
	evidence := Evidence{SpeakerIdentifications: []SpeakerIdentificationCandidate{
		speakerCandidate("speaker-1", "profile-a", 1, now),
		speakerCandidate("speaker-2", "profile-b", 0.1, now),
	}}
	records := []biometric.Record{
		activeEnrollment("profile-a", readiness.SpeakerIdentification, "speaker-model.v1", now),
	}
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
	reversed.SpeakerIdentifications = []SpeakerIdentificationCandidate{
		evidence.SpeakerIdentifications[1], evidence.SpeakerIdentifications[0],
	}
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
	validPermissions := permissionSnapshot(
		now, privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness,
	)
	validCatalog := biometric.Snapshot{Revision: 1, Records: []biometric.Record{
		activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now),
	}}
	tests := []struct {
		name        string
		policy      Policy
		evidence    Evidence
		permissions privacy.Snapshot
		catalog     biometric.Snapshot
	}{
		{name: "invalid evidence", policy: testPolicy(), evidence: Evidence{}, permissions: validPermissions, catalog: validCatalog},
		{name: "duplicate permission", policy: testPolicy(), evidence: validFaceEvidence(now), permissions: privacy.Snapshot{Revision: 1, Grants: []privacy.Grant{
			{Permission: privacy.FaceIdentification, Enabled: true, UpdatedAt: now},
			{Permission: privacy.FaceIdentification, Enabled: true, UpdatedAt: now},
		}}, catalog: validCatalog},
		{name: "duplicate enrollment", policy: testPolicy(), evidence: validFaceEvidence(now), permissions: validPermissions, catalog: biometric.Snapshot{Revision: 1, Records: []biometric.Record{
			activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now),
			activeEnrollment("profile-a", readiness.FaceIdentification, "face-model.v1", now),
		}}},
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

func assertAnonymousReason(t *testing.T, resolution Resolution, reason Reason) {
	t.Helper()
	if resolution.Assurance != Anonymous || resolution.ProfileRef != "" || resolution.Reason != reason || resolution.PolicyVersion != testPolicy().Version {
		t.Fatalf("resolution = %#v, want ANONYMOUS/%s", resolution, reason)
	}
}

func activeEnrollment(profile string, capability readiness.CapabilityKind, modelVersion string, at time.Time) biometric.Record {
	return biometric.Record{
		ProfileRef: profile, Capability: capability,
		Consented: true, ConsentUpdatedAt: at,
		TemplateRef:  "template-" + profile + "-" + string(capability),
		ModelVersion: modelVersion, Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
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
