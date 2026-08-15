package biometric

import (
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestCapabilityAuthorizedRequiresEveryGlobalPrerequisite(t *testing.T) {
	updatedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		capability    readiness.CapabilityKind
		prerequisites []privacy.Permission
	}{
		{readiness.FaceDetection, []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection}},
		{readiness.FaceIdentification, []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification}},
		{readiness.FaceLiveness, []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceLiveness}},
		{readiness.SpeakerIdentification, []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification}},
		{readiness.SpeakerVerification, []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerVerification}},
	}
	for _, test := range tests {
		t.Run(string(test.capability), func(t *testing.T) {
			complete := enabledPermissionSnapshot(updatedAt, test.prerequisites...)
			got, err := CapabilityAuthorized(complete, test.capability)
			if err != nil || !got {
				t.Fatalf("CapabilityAuthorized(complete) = %t, %v, want true", got, err)
			}

			for _, missing := range test.prerequisites {
				t.Run("missing_"+string(missing), func(t *testing.T) {
					incomplete := enabledPermissionSnapshot(updatedAt, test.prerequisites...)
					for index := range incomplete.Grants {
						if incomplete.Grants[index].Permission == missing {
							incomplete.Grants[index].Enabled = false
							incomplete.Grants[index].UpdatedAt = time.Time{}
						}
					}
					got, queryErr := CapabilityAuthorized(incomplete, test.capability)
					if queryErr != nil || got {
						t.Fatalf("CapabilityAuthorized(missing %s) = %t, %v, want false", missing, got, queryErr)
					}
				})
			}
		})
	}
}

func TestCapabilityAuthorizedRejectsInvalidPermissionSnapshots(t *testing.T) {
	updatedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot privacy.Snapshot
	}{
		{
			name: "unknown permission",
			snapshot: privacy.Snapshot{Revision: 1, Grants: []privacy.Grant{{
				Permission: privacy.Permission("UNKNOWN"), Enabled: true, UpdatedAt: updatedAt,
			}}},
		},
		{
			name: "duplicate permission",
			snapshot: privacy.Snapshot{Revision: 1, Grants: []privacy.Grant{
				{Permission: privacy.CameraCapture, Enabled: true, UpdatedAt: updatedAt},
				{Permission: privacy.CameraCapture, Enabled: true, UpdatedAt: updatedAt},
			}},
		},
		{
			name: "enabled permission without update time",
			snapshot: privacy.Snapshot{Revision: 1, Grants: []privacy.Grant{{
				Permission: privacy.CameraCapture, Enabled: true,
			}}},
		},
		{
			name: "changed revision zero",
			snapshot: privacy.Snapshot{Grants: []privacy.Grant{{
				Permission: privacy.CameraCapture, Enabled: true, UpdatedAt: updatedAt,
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CapabilityAuthorized(test.snapshot, readiness.FaceIdentification); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("CapabilityAuthorized() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestCapabilityAuthorizedRejectsNonBiometricCapability(t *testing.T) {
	if _, err := CapabilityAuthorized(privacy.Snapshot{}, readiness.PersonPresence); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("CapabilityAuthorized(non-biometric) error = %v, want InvalidInput", err)
	}
}

func enabledPermissionSnapshot(updatedAt time.Time, enabled ...privacy.Permission) privacy.Snapshot {
	wanted := make(map[privacy.Permission]struct{}, len(enabled))
	for _, permission := range enabled {
		wanted[permission] = struct{}{}
	}
	snapshot := privacy.Snapshot{Revision: 1, Grants: make([]privacy.Grant, 0, len(privacy.AllPermissions()))}
	for _, permission := range privacy.AllPermissions() {
		grant := privacy.Grant{Permission: permission}
		if _, ok := wanted[permission]; ok {
			grant.Enabled = true
			grant.UpdatedAt = updatedAt
		}
		snapshot.Grants = append(snapshot.Grants, grant)
	}
	return snapshot
}
