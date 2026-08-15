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

func TestDeletionCoordinatorRemovesRetiredAndCurrentVaultTemplates(t *testing.T) {
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
		TemplateRef: "face-v1", ModelVersion: "face.model.v1",
	}
	registrationV2 := registrationV1
	registrationV2.TemplateRef = "face-v2"
	registrationV2.ModelVersion = "face.model.v2"
	descriptorV1 := descriptorFromRegistration(registrationV1)
	descriptorV2 := descriptorFromRegistration(registrationV2)
	if err := vault.Store(ctx, descriptorV1, []byte("opaque-face-template-v1")); err != nil {
		t.Fatalf("Store(v1) error = %v", err)
	}
	if err := vault.Store(ctx, descriptorV2, []byte("opaque-face-template-v2")); err != nil {
		t.Fatalf("Store(v2) error = %v", err)
	}
	if _, err := catalog.GrantConsent(ctx, biometric.ConsentCommand{
		ProfileRef: registrationV1.ProfileRef, Capability: registrationV1.Capability,
	}); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	if _, err := catalog.Register(ctx, registrationV1); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := catalog.Replace(ctx, registrationV2); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	coordinator, err := biometric.NewDeletionCoordinator(catalog, vault)
	if err != nil {
		t.Fatalf("NewDeletionCoordinator() error = %v", err)
	}

	cleaned, err := coordinator.RetryPendingDeletes(ctx)
	if err != nil {
		t.Fatalf("RetryPendingDeletes() error = %v", err)
	}
	if cleaned.Records[0].PendingDelete != nil {
		t.Fatalf("retired template remained pending: %#v", cleaned.Records[0])
	}
	if _, err := vault.Open(ctx, descriptorV1); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(retired) error = %v, want Unavailable", err)
	}
	if opened, err := vault.Open(ctx, descriptorV2); err != nil || string(opened) != "opaque-face-template-v2" {
		t.Fatalf("Open(active) = %q, %v", opened, err)
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
