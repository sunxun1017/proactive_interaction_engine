package biometric

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

const testStoreOperationID = "00112233445566778899aabbccddeeff"

func TestCatalogDefaultsClosedAndIntersectsGlobalPermission(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	policy, err := service.ReadinessPolicy(globalPermissions(
		privacy.CameraCapture,
		privacy.FaceDetection,
		privacy.FaceIdentification,
	), "profile-a")
	if err != nil {
		t.Fatalf("ReadinessPolicy() error = %v", err)
	}
	want := readiness.BiometricPolicySnapshot{
		Enabled: true, Authorized: []readiness.CapabilityKind{readiness.FaceDetection},
	}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("policy = %#v, want %#v", policy, want)
	}
	if current := service.Current(); current.Revision != 0 || len(current.Records) != 0 {
		t.Fatalf("initial snapshot = %#v", current)
	}
}

func TestCatalogReadinessRequiresCaptureAndDetectionPrerequisites(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	for _, capability := range []readiness.CapabilityKind{readiness.FaceIdentification, readiness.SpeakerIdentification} {
		if _, err := service.GrantConsent(ctx, ConsentCommand{ProfileRef: "profile-a", Capability: capability}); err != nil {
			t.Fatalf("GrantConsent(%s) error = %v", capability, err)
		}
		activateRegistrationForTest(t, service, Registration{
			ProfileRef: "profile-a", Capability: capability,
			TemplateRef: "template-" + string(capability), ModelVersion: "model.v1",
		})
	}

	for _, test := range []struct {
		name       string
		enabled    []privacy.Permission
		authorized []readiness.CapabilityKind
		enrolled   []readiness.CapabilityKind
	}{
		{name: "face identity without capture", enabled: []privacy.Permission{privacy.FaceDetection, privacy.FaceIdentification}},
		{name: "face identity without detection", enabled: []privacy.Permission{privacy.CameraCapture, privacy.FaceIdentification}},
		{
			name:       "complete face chain",
			enabled:    []privacy.Permission{privacy.CameraCapture, privacy.FaceDetection, privacy.FaceIdentification},
			authorized: []readiness.CapabilityKind{readiness.FaceDetection, readiness.FaceIdentification},
			enrolled:   []readiness.CapabilityKind{readiness.FaceIdentification},
		},
		{name: "speaker identity without capture", enabled: []privacy.Permission{privacy.SpeakerIdentification}},
		{
			name:       "complete speaker chain",
			enabled:    []privacy.Permission{privacy.MicrophoneCapture, privacy.SpeakerIdentification},
			authorized: []readiness.CapabilityKind{readiness.SpeakerIdentification},
			enrolled:   []readiness.CapabilityKind{readiness.SpeakerIdentification},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := service.ReadinessPolicy(globalPermissions(test.enabled...), "profile-a")
			if err != nil {
				t.Fatalf("ReadinessPolicy() error = %v", err)
			}
			if !reflect.DeepEqual(policy.Authorized, test.authorized) || !reflect.DeepEqual(policy.Enrolled, test.enrolled) {
				t.Fatalf("policy = %#v, want authorized=%#v enrolled=%#v", policy, test.authorized, test.enrolled)
			}
		})
	}
}

func TestCatalogKeepsProfileCapabilitiesIndependentAndSnapshotsImmutable(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	for _, command := range []ConsentCommand{
		{ProfileRef: "profile-b", Capability: readiness.FaceIdentification},
		{ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification},
	} {
		if _, err := service.GrantConsent(ctx, command); err != nil {
			t.Fatalf("GrantConsent(%#v) error = %v", command, err)
		}
	}
	for _, registration := range []Registration{
		{ProfileRef: "profile-b", Capability: readiness.FaceIdentification, TemplateRef: "template-face-b", ModelVersion: "face.v1"},
		{ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification, TemplateRef: "template-speaker-a", ModelVersion: "speaker.v1"},
	} {
		activateRegistrationForTest(t, service, registration)
	}

	policy, err := service.ReadinessPolicy(globalPermissions(
		privacy.MicrophoneCapture,
		privacy.FaceIdentification,
		privacy.SpeakerIdentification,
	), "profile-a")
	if err != nil {
		t.Fatalf("ReadinessPolicy() error = %v", err)
	}
	want := readiness.BiometricPolicySnapshot{
		Enabled:    true,
		Authorized: []readiness.CapabilityKind{readiness.SpeakerIdentification},
		Enrolled:   []readiness.CapabilityKind{readiness.SpeakerIdentification},
	}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("profile-a policy = %#v, want %#v", policy, want)
	}

	snapshot := service.Current()
	if len(snapshot.Records) != 2 || snapshot.Records[0].ProfileRef != "profile-a" || snapshot.Records[1].ProfileRef != "profile-b" {
		t.Fatalf("records are not in stable order: %#v", snapshot.Records)
	}
	snapshot.Records[0].TemplateRef = "mutated"
	if service.Current().Records[0].TemplateRef == "mutated" {
		t.Fatal("Current() exposed mutable catalog state")
	}
}

func TestCatalogRejectsRegistrationWithoutProfileConsent(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	_, err := service.PrepareRegister(context.Background(), Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "template-a", ModelVersion: "face.v1",
	}, testStoreOperationID)
	if !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("Register() error = %v, want PermissionDenied", err)
	}
}

func TestCatalogRequiresExplicitReplaceAndKeepsExactRegisterIdempotent(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	ctx := context.Background()
	consent := ConsentCommand{ProfileRef: "profile-a", Capability: readiness.SpeakerVerification}
	if _, err := service.GrantConsent(ctx, consent); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	first := Registration{
		ProfileRef: "profile-a", Capability: readiness.SpeakerVerification,
		TemplateRef: "template-v1", ModelVersion: "speaker.v1",
	}
	registered := activateRegistrationForTest(t, service, first)
	again, err := service.PrepareRegister(ctx, first, testStoreOperationID)
	if err != nil || again.Revision != registered.Revision {
		t.Fatalf("idempotent Register() = %#v, %v", again, err)
	}
	second := first
	second.TemplateRef = "template-v2"
	second.ModelVersion = "speaker.v2"
	if _, err := service.PrepareRegister(ctx, second, testStoreOperationID); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("implicit replacement error = %v, want InvalidInput", err)
	}
	replaced := replaceRegistrationForTest(t, service, second)
	if got := replaced.Records[0]; got.TemplateRef != "template-v2" || got.ModelVersion != "speaker.v2" || got.Status != EnrollmentActive || got.PendingDelete == nil || got.PendingDelete.TemplateRef != "template-v1" || got.PendingDelete.ModelVersion != "speaker.v1" {
		t.Fatalf("replaced record = %#v", got)
	}
	restarted := newTestService(t, repository)
	returned := restarted.Current()
	returned.Records[0].PendingDelete.TemplateRef = "mutated"
	if got := restarted.Current().Records[0].PendingDelete.TemplateRef; got != "template-v1" {
		t.Fatalf("Current() exposed mutable pending deletion: %q", got)
	}
	key := EnrollmentKey{
		ProfileRef: "profile-a", Capability: readiness.SpeakerVerification,
	}
	beforeWrongConfirmation := restarted.Current()
	if _, err := restarted.ConfirmRetiredDelete(ctx, key, TemplateReference{
		TemplateRef: "wrong-template", ModelVersion: "speaker.v1",
	}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ConfirmRetiredDelete(wrong ref) error = %v, want InvalidInput", err)
	}
	if _, err := restarted.ConfirmRetiredDelete(ctx, key, TemplateReference{
		TemplateRef: "template-v1", ModelVersion: "speaker.wrong",
	}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("ConfirmRetiredDelete(wrong model) error = %v, want InvalidInput", err)
	}
	if after := restarted.Current(); !reflect.DeepEqual(after, beforeWrongConfirmation) {
		t.Fatalf("wrong retired reference changed state: %#v != %#v", after, beforeWrongConfirmation)
	}
	third := second
	third.TemplateRef = "template-v3"
	if _, err := restarted.PrepareReplace(ctx, third, testStoreOperationID); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("Replace() with pending deletion error = %v, want PolicyBlocked", err)
	}
	cleaned, err := restarted.ConfirmRetiredDelete(ctx, key, TemplateReference{
		TemplateRef: "template-v1", ModelVersion: "speaker.v1",
	})
	if err != nil {
		t.Fatalf("ConfirmRetiredDelete() error = %v", err)
	}
	if cleaned.Records[0].PendingDelete != nil {
		t.Fatalf("pending deletion after confirmation = %#v", cleaned.Records[0].PendingDelete)
	}
}

func TestCatalogRevocationAndDeletionAreFailClosedAndRecoverable(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	ctx := context.Background()
	command := ConsentCommand{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}
	registration := Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "template-a", ModelVersion: "face.v1",
	}
	if _, err := service.GrantConsent(ctx, command); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	activateRegistrationForTest(t, service, registration)
	if _, err := service.RevokeConsent(ctx, command); err != nil {
		t.Fatalf("RevokeConsent() error = %v", err)
	}
	if _, err := service.RequestDelete(ctx, EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}); err != nil {
		t.Fatalf("RequestDelete() error = %v", err)
	}

	restarted := newTestService(t, repository)
	policy, err := restarted.ReadinessPolicy(globalPermissions(privacy.FaceIdentification), "profile-a")
	if err != nil {
		t.Fatalf("ReadinessPolicy() error = %v", err)
	}
	if len(policy.Authorized) != 0 || len(policy.Enrolled) != 0 {
		t.Fatalf("pending deletion policy = %#v, want fail closed", policy)
	}
	record := restarted.Current().Records[0]
	if record.Status != EnrollmentDeletePending || record.TemplateRef != "template-a" {
		t.Fatalf("recovered pending deletion = %#v", record)
	}
	deleted, err := restarted.ConfirmDelete(ctx, EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.FaceIdentification})
	if err != nil {
		t.Fatalf("ConfirmDelete() error = %v", err)
	}
	if got := deleted.Records[0]; got.TemplateRef != "" || got.ModelVersion != "" || got.Status != EnrollmentNone {
		t.Fatalf("confirmed deletion record = %#v", got)
	}
}

func TestCatalogDeleteProfileRevokesAndQueuesEveryEnrollment(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	for index, capability := range []readiness.CapabilityKind{
		readiness.FaceIdentification,
		readiness.SpeakerIdentification,
		readiness.SpeakerVerification,
	} {
		command := ConsentCommand{ProfileRef: "profile-a", Capability: capability}
		if _, err := service.GrantConsent(ctx, command); err != nil {
			t.Fatalf("GrantConsent(%s) error = %v", capability, err)
		}
		activateRegistrationForTest(t, service, Registration{
			ProfileRef: "profile-a", Capability: capability,
			TemplateRef: fmt.Sprintf("template-%d", index), ModelVersion: "model.v1",
		})
	}
	snapshot, err := service.DeleteProfile(ctx, "profile-a")
	if err != nil {
		t.Fatalf("DeleteProfile() error = %v", err)
	}
	for _, record := range snapshot.Records {
		if record.Consented || record.Status != EnrollmentDeletePending || record.TemplateRef == "" {
			t.Fatalf("profile deletion record = %#v", record)
		}
	}
	policy, err := service.ReadinessPolicy(globalPermissions(
		privacy.FaceIdentification,
		privacy.SpeakerIdentification,
		privacy.SpeakerVerification,
	), "profile-a")
	if err != nil {
		t.Fatalf("ReadinessPolicy() error = %v", err)
	}
	if len(policy.Authorized) != 0 || len(policy.Enrolled) != 0 {
		t.Fatalf("deleted profile policy = %#v", policy)
	}
}

func TestCatalogRejectsInvalidProfileAndCapability(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	for _, command := range []ConsentCommand{
		{ProfileRef: " profile-a", Capability: readiness.FaceIdentification},
		{ProfileRef: "profile-a", Capability: readiness.FaceDetection},
		{ProfileRef: "profile-a", Capability: readiness.CapabilityKind("UNKNOWN")},
	} {
		if _, err := service.GrantConsent(context.Background(), command); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("GrantConsent(%#v) error = %v, want InvalidInput", command, err)
		}
	}
}

func TestCatalogRejectsTemplateReferenceReuseAcrossProfiles(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	ctx := context.Background()
	for _, profileRef := range []string{"profile-a", "profile-b"} {
		if _, err := service.GrantConsent(ctx, ConsentCommand{ProfileRef: profileRef, Capability: readiness.FaceIdentification}); err != nil {
			t.Fatalf("GrantConsent(%s) error = %v", profileRef, err)
		}
	}
	activateRegistrationForTest(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "shared-template", ModelVersion: "face.v1",
	})
	before := service.Current()
	_, err := service.PrepareRegister(ctx, Registration{
		ProfileRef: "profile-b", Capability: readiness.FaceIdentification,
		TemplateRef: "shared-template", ModelVersion: "face.v1",
	}, testStoreOperationID)
	if !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Register(profile-b) error = %v, want InvalidInput", err)
	}
	if after := service.Current(); !reflect.DeepEqual(after, before) {
		t.Fatalf("duplicate template reference changed state: %#v != %#v", after, before)
	}
}

func TestCatalogSerializesConcurrentConsentChanges(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	const changes = 24
	errorsSeen := make(chan error, changes)
	var wait sync.WaitGroup
	for index := 0; index < changes; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := service.GrantConsent(context.Background(), ConsentCommand{
				ProfileRef: fmt.Sprintf("profile-%02d", index), Capability: readiness.FaceIdentification,
			})
			errorsSeen <- err
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent GrantConsent() error = %v", err)
		}
	}
	snapshot := service.Current()
	if snapshot.Revision != changes || len(snapshot.Records) != changes {
		t.Fatalf("concurrent snapshot = revision %d records %d", snapshot.Revision, len(snapshot.Records))
	}
}

func TestCatalogDoesNotPublishFailedPersistence(t *testing.T) {
	repository := &memoryRepository{saveErr: errors.New("disk unavailable")}
	service := newTestService(t, repository)
	_, err := service.GrantConsent(context.Background(), ConsentCommand{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
	})
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("GrantConsent() error = %v, want Unavailable", err)
	}
	if current := service.Current(); current.Revision != 0 || len(current.Records) != 0 {
		t.Fatalf("state published after failed save: %#v", current)
	}
}

func TestCatalogReplacementSaveFailureFailsClosedUntilReload(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	ctx := context.Background()
	command := ConsentCommand{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}
	if _, err := service.GrantConsent(ctx, command); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	activateRegistrationForTest(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "template-v1", ModelVersion: "face.v1",
	})
	before := service.Current()
	repository.setSaveError(errors.New("disk unavailable"))
	_, err := service.PrepareReplace(ctx, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "template-v2", ModelVersion: "face.v2",
	}, testStoreOperationID)
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Replace() error = %v, want Unavailable", err)
	}
	if after := service.Current(); after.Revision != 0 || len(after.Records) != 0 {
		t.Fatalf("catalog did not fail closed after uncertain save: %#v", after)
	}
	repository.setSaveError(nil)
	reloaded, err := service.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if !reflect.DeepEqual(reloaded, before) {
		t.Fatalf("Reload() = %#v, want durable %#v", reloaded, before)
	}
}

func activateRegistrationForTest(t *testing.T, service *Service, registration Registration) Snapshot {
	t.Helper()
	ctx := context.Background()
	if _, err := service.PrepareRegister(ctx, registration, testStoreOperationID); err != nil {
		t.Fatalf("PrepareRegister(%#v) error = %v", registration, err)
	}
	committed, err := service.CommitPrepared(ctx, registration, testStoreOperationID)
	if err != nil {
		t.Fatalf("CommitPrepared(%#v) error = %v", registration, err)
	}
	return committed
}

func replaceRegistrationForTest(t *testing.T, service *Service, registration Registration) Snapshot {
	t.Helper()
	ctx := context.Background()
	if _, err := service.PrepareReplace(ctx, registration, testStoreOperationID); err != nil {
		t.Fatalf("PrepareReplace(%#v) error = %v", registration, err)
	}
	committed, err := service.CommitPrepared(ctx, registration, testStoreOperationID)
	if err != nil {
		t.Fatalf("CommitPrepared(%#v) error = %v", registration, err)
	}
	return committed
}

func newTestService(t *testing.T, repository Repository) *Service {
	t.Helper()
	service, err := New(context.Background(), repository, engineclock.NewFake(testTime()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service
}

func globalPermissions(enabled ...privacy.Permission) privacy.Snapshot {
	selected := make(map[privacy.Permission]struct{}, len(enabled))
	for _, permission := range enabled {
		selected[permission] = struct{}{}
	}
	snapshot := privacy.Snapshot{Revision: 1, Grants: make([]privacy.Grant, 0, len(privacy.AllPermissions()))}
	for _, permission := range privacy.AllPermissions() {
		_, active := selected[permission]
		grant := privacy.Grant{Permission: permission, Enabled: active}
		if active {
			grant.UpdatedAt = testTime()
		}
		snapshot.Grants = append(snapshot.Grants, grant)
	}
	return snapshot
}

func testTime() time.Time {
	return time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
}

type memoryRepository struct {
	mu                 sync.Mutex
	current            Snapshot
	saveErr            error
	persistBeforeError bool
}

func (r *memoryRepository) Load(context.Context) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSnapshot(r.current), nil
}

func (r *memoryRepository) Save(_ context.Context, expected uint64, next Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current.Revision != expected {
		return errors.New("revision conflict")
	}
	if r.saveErr != nil && !r.persistBeforeError {
		return r.saveErr
	}
	r.current = cloneSnapshot(next)
	return r.saveErr
}

func (r *memoryRepository) setSaveError(err error) {
	r.mu.Lock()
	r.saveErr = err
	r.mu.Unlock()
}
