package biometricvault

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestTemplateLifecycleStoresReplacesAndDeletesEncryptedTemplates(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "vault")
	keyProvider := staticKeyProvider{key: testKey(21)}
	vault, err := New(root, keyProvider)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	repository, err := NewCatalogRepository(root, keyProvider)
	if err != nil {
		t.Fatalf("NewCatalogRepository() error = %v", err)
	}
	catalog, err := biometric.New(ctx, repository, engineclock.NewFake(time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("biometric.New() error = %v", err)
	}
	registrationV1 := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-v1", ModelVersion: FaceInferenceProfileID,
	}
	registrationV2 := registrationV1
	registrationV2.TemplateRef = "face-v2"
	descriptorV1 := descriptorFromRegistration(registrationV1)
	descriptorV2 := descriptorFromRegistration(registrationV2)
	if _, err := catalog.GrantConsent(ctx, biometric.ConsentCommand{
		ProfileRef: registrationV1.ProfileRef, Capability: registrationV1.Capability,
	}); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	coordinator, err := NewTemplateLifecycleCoordinator(catalog, vault)
	if err != nil {
		t.Fatalf("NewTemplateLifecycleCoordinator() error = %v", err)
	}
	if _, err := coordinator.Recover(ctx); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if _, err := coordinator.Register(ctx, registrationV1, validFaceTemplatePayload()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	cleaned, err := coordinator.Replace(ctx, registrationV2, validFaceTemplatePayload())
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if cleaned.Records[0].PendingDelete != nil {
		t.Fatalf("retired template remained pending: %#v", cleaned.Records[0])
	}
	if _, err := vault.Open(ctx, descriptorV1); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(retired) error = %v, want Unavailable", err)
	}
	if opened, err := vault.Open(ctx, descriptorV2); err != nil || len(opened) != faceTemplateBytes {
		t.Fatalf("Open(active) bytes = %d, %v", len(opened), err)
	}

	deleted, err := coordinator.Delete(ctx, biometric.EnrollmentKey{
		ProfileRef: registrationV2.ProfileRef, Capability: registrationV2.Capability,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := deleted.Records[0]; got.Status != biometric.EnrollmentNone || got.TemplateRef != "" {
		t.Fatalf("deleted catalog record = %#v", got)
	}
	if _, err := vault.Open(ctx, descriptorV2); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(deleted active) error = %v, want Unavailable", err)
	}
}

func descriptorFromRegistration(registration biometric.Registration) Descriptor {
	return Descriptor{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
		TemplateRef: registration.TemplateRef, ModelVersion: registration.ModelVersion,
	}
}
