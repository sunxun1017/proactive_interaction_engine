package biometric

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestCatalogStagesRegistrationBeforeActivation(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	registration := Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template-a", ModelVersion: "opencv-sface-2021dec-profile-v1",
	}
	if _, err := service.GrantConsent(ctx, ConsentCommand{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
	}); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}

	prepared, err := service.PrepareRegister(ctx, registration, testStoreOperationID)
	if err != nil {
		t.Fatalf("PrepareRegister() error = %v", err)
	}
	record := prepared.Records[0]
	if record.Status != EnrollmentNone || record.PendingStore == nil ||
		record.PendingStore.TemplateRef != registration.TemplateRef ||
		record.PendingStore.StoreOperationID != testStoreOperationID {
		t.Fatalf("prepared record = %#v", record)
	}
	committed, err := service.CommitPrepared(ctx, registration, testStoreOperationID)
	if err != nil {
		t.Fatalf("CommitPrepared() error = %v", err)
	}
	record = committed.Records[0]
	if record.Status != EnrollmentActive || record.TemplateRef != registration.TemplateRef || record.PendingStore != nil {
		t.Fatalf("committed record = %#v", record)
	}
}

func TestCatalogPreparedStoreRequiresExactOperationID(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	registration := Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template", ModelVersion: "face-profile-v1",
	}
	if _, err := service.GrantConsent(ctx, ConsentCommand{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareRegister(ctx, registration, testStoreOperationID); err != nil {
		t.Fatal(err)
	}
	wrongOperationID := "ffeeddccbbaa99887766554433221100"
	if _, err := service.PrepareRegister(ctx, registration, wrongOperationID); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("PrepareRegister(wrong operation) error = %v, want StaleInput", err)
	}
	if _, err := service.CommitPrepared(ctx, registration, wrongOperationID); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("CommitPrepared(wrong operation) error = %v, want StaleInput", err)
	}
	if _, err := service.AbortPrepared(ctx, registration, wrongOperationID); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("AbortPrepared(wrong operation) error = %v, want StaleInput", err)
	}
	if got := service.Current().Records[0].PendingStore; got == nil || got.StoreOperationID != testStoreOperationID {
		t.Fatalf("pending store after wrong operation calls = %#v", got)
	}
}

func TestCatalogPreparedReplacementKeepsOldActiveUntilCommit(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	command := ConsentCommand{ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification}
	if _, err := service.GrantConsent(ctx, command); err != nil {
		t.Fatal(err)
	}
	old := Registration{ProfileRef: command.ProfileRef, Capability: command.Capability, TemplateRef: "speaker-old", ModelVersion: "model-old"}
	activateRegistrationForTest(t, service, old)
	replacement := Registration{ProfileRef: command.ProfileRef, Capability: command.Capability, TemplateRef: "speaker-new", ModelVersion: "model-new"}

	prepared, err := service.PrepareReplace(ctx, replacement, testStoreOperationID)
	if err != nil {
		t.Fatalf("PrepareReplace() error = %v", err)
	}
	if got := prepared.Records[0]; got.Status != EnrollmentActive || got.TemplateRef != old.TemplateRef || got.PendingStore == nil {
		t.Fatalf("prepared replacement = %#v", got)
	}
	committed, err := service.CommitPrepared(ctx, replacement, testStoreOperationID)
	if err != nil {
		t.Fatalf("CommitPrepared() error = %v", err)
	}
	if got := committed.Records[0]; got.TemplateRef != replacement.TemplateRef || got.PendingStore != nil || got.PendingDelete == nil || got.PendingDelete.TemplateRef != old.TemplateRef {
		t.Fatalf("committed replacement = %#v", got)
	}
}

func TestCatalogPreparedStoreCannotSurviveRevokeAndRegrant(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	command := ConsentCommand{ProfileRef: "profile-a", Capability: readiness.SpeakerVerification}
	registration := Registration{
		ProfileRef: command.ProfileRef, Capability: command.Capability,
		TemplateRef: "speaker-template", ModelVersion: "speaker-profile-v1",
	}
	if _, err := service.GrantConsent(ctx, command); err != nil {
		t.Fatal(err)
	}
	prepared, err := service.PrepareRegister(ctx, registration, testStoreOperationID)
	if err != nil {
		t.Fatal(err)
	}
	preparedVersion := prepared.Records[0].PendingStore.ConsentVersion
	if _, err := service.RevokeConsent(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GrantConsent(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CommitPrepared(ctx, registration, testStoreOperationID); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("CommitPrepared() error = %v, want StaleInput", err)
	}
	current := service.Current().Records[0]
	if current.Status != EnrollmentNone || current.PendingStore == nil || current.PendingStore.ConsentVersion != preparedVersion || current.ConsentVersion == preparedVersion {
		t.Fatalf("stale prepared registration = %#v", current)
	}
	if _, err := service.AbortPrepared(ctx, registration, testStoreOperationID); err != nil {
		t.Fatalf("AbortPrepared() error = %v", err)
	}
}

func TestCatalogSaveErrorRequiresReloadAndHidesOldMemory(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	repository.persistBeforeError = true
	repository.setSaveError(errors.New("directory sync failed"))

	_, err := service.GrantConsent(context.Background(), ConsentCommand{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
	})
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("GrantConsent() error = %v, want Unavailable", err)
	}
	if current := service.Current(); current.Revision != 0 || len(current.Records) != 0 {
		t.Fatalf("Current() while reload required = %#v, want empty", current)
	}
	if _, err := service.ReadinessPolicy(globalPermissions(privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification), "profile-a"); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("ReadinessPolicy() error = %v, want Unavailable", err)
	}
	if _, err := service.GrantConsent(context.Background(), ConsentCommand{
		ProfileRef: "profile-b", Capability: readiness.FaceIdentification,
	}); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("mutation before Reload() error = %v, want Unavailable", err)
	}

	repository.setSaveError(nil)
	reloaded, err := service.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if reloaded.Revision != 1 || len(reloaded.Records) != 1 || !reloaded.Records[0].Consented {
		t.Fatalf("Reload() = %#v", reloaded)
	}
	if got := service.Current(); !reflect.DeepEqual(got, reloaded) {
		t.Fatalf("Current() after Reload = %#v, want %#v", got, reloaded)
	}
}

func TestCatalogRejectsInvalidPersistedSnapshotAsAdapterData(t *testing.T) {
	repository := &memoryRepository{current: Snapshot{
		Revision: 1,
		Records: []Record{{
			ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
			Consented: true, ConsentUpdatedAt: testTime(), Status: EnrollmentNone,
		}},
	}}
	_, err := New(context.Background(), repository, engineclock.NewFake(testTime()))
	if !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("New(invalid persisted snapshot) error = %v, want AdapterRejected", err)
	}
}
