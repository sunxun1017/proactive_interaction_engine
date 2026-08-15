package biometric

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestDeletionCoordinatorDeletesCurrentBeforeRetiredTemplate(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	key := EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}
	coordinatorEnroll(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "face-v1", ModelVersion: "face.model.v1",
	})
	replaceRegistrationForTest(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "face-v2", ModelVersion: "face.model.v2",
	})

	deleter := &recordingTemplateDeleter{}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	snapshot, err := coordinator.Delete(context.Background(), key)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	wantCalls := []Registration{
		{ProfileRef: "profile-a", Capability: readiness.FaceIdentification, TemplateRef: "face-v2", ModelVersion: "face.model.v2"},
		{ProfileRef: "profile-a", Capability: readiness.FaceIdentification, TemplateRef: "face-v1", ModelVersion: "face.model.v1"},
	}
	if got := deleter.Calls(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("Delete() calls = %#v, want %#v", got, wantCalls)
	}
	if got := snapshot.Records[0]; got.Status != EnrollmentNone || got.TemplateRef != "" || got.ModelVersion != "" || got.PendingDelete != nil {
		t.Fatalf("deleted record = %#v, want no template metadata", got)
	}
}

func TestDeletionCoordinatorKeepsFailedPendingAndContinues(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	key := EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.SpeakerVerification}
	coordinatorEnroll(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "speaker-v1", ModelVersion: "speaker.model.v1",
	})
	replaceRegistrationForTest(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "speaker-v2", ModelVersion: "speaker.model.v2",
	})

	deleter := &recordingTemplateDeleter{failures: map[string]error{
		"speaker-v2": fault.New(fault.Unavailable, "fake delete", errors.New("vault offline")),
	}}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	snapshot, err := coordinator.Delete(context.Background(), key)
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Delete() error = %v, want Unavailable", err)
	}
	if got := deleter.TemplateRefs(); !reflect.DeepEqual(got, []string{"speaker-v2", "speaker-v1"}) {
		t.Fatalf("Delete() calls = %v, want current then retired", got)
	}
	record := snapshot.Records[0]
	if record.Status != EnrollmentDeletePending || record.TemplateRef != "speaker-v2" {
		t.Fatalf("failed current deletion record = %#v", record)
	}
	if record.PendingDelete != nil {
		t.Fatalf("successful retired deletion remained pending: %#v", record.PendingDelete)
	}

	deleter.SetFailures(nil)
	retried, err := coordinator.RetryPendingDeletes(context.Background())
	if err != nil {
		t.Fatalf("RetryPendingDeletes() error = %v", err)
	}
	if got := retried.Records[0]; got.Status != EnrollmentNone || got.PendingDelete != nil {
		t.Fatalf("retried record = %#v, want fully deleted", got)
	}
}

func TestDeletionCoordinatorDeleteProfileUsesStableOrderAndStopsAfterUncertainConfirmation(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	coordinatorEnroll(t, service, Registration{
		ProfileRef: "profile-b", Capability: readiness.FaceIdentification,
		TemplateRef: "b-face", ModelVersion: "face.model.v1",
	})
	coordinatorEnroll(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification,
		TemplateRef: "a-speaker", ModelVersion: "speaker.model.v1",
	})
	coordinatorEnroll(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "a-face-v1", ModelVersion: "face.model.v1",
	})
	replaceRegistrationForTest(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "a-face-v2", ModelVersion: "face.model.v2",
	})

	deleter := &recordingTemplateDeleter{failures: map[string]error{
		"a-face-v2": fault.New(fault.AdapterRejected, "fake delete", errors.New("tampered")),
		"a-speaker": fault.New(fault.Unavailable, "fake delete", errors.New("busy")),
	}, onDelete: func(registration Registration) {
		if registration.TemplateRef == "a-face-v1" {
			repository.setSaveError(errors.New("catalog confirmation offline"))
		}
	}}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	snapshot, err := coordinator.DeleteProfile(context.Background(), "profile-a")
	if !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("DeleteProfile() error = %v, want first stable AdapterRejected", err)
	}
	if got := deleter.TemplateRefs(); !reflect.DeepEqual(got, []string{"a-face-v2", "a-face-v1"}) {
		t.Fatalf("DeleteProfile() calls = %v, want stable current/retired order followed by immediate stop", got)
	}
	if snapshot.Revision != 0 || len(snapshot.Records) != 0 {
		t.Fatalf("snapshot after uncertain confirmation = %#v, want fail-closed empty", snapshot)
	}
	repository.setSaveError(nil)
	snapshot, err = service.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	for _, record := range snapshot.Records {
		if record.ProfileRef == "profile-a" && record.Consented {
			t.Fatalf("profile consent remained enabled after deletion request: %#v", record)
		}
		if record.ProfileRef == "profile-a" && record.Capability == readiness.FaceIdentification && record.PendingDelete == nil {
			t.Fatalf("failed retired confirmation did not remain pending: %#v", record)
		}
		if record.ProfileRef == "profile-b" && record.Status != EnrollmentActive {
			t.Fatalf("unrelated profile changed: %#v", record)
		}
	}
}

func TestDeletionCoordinatorDoesNotCallVaultWhenDeleteRequestFails(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	key := EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}
	coordinatorEnroll(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "face-v1", ModelVersion: "face.model.v1",
	})
	repository.setSaveError(errors.New("catalog offline"))

	deleter := &recordingTemplateDeleter{}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	if _, err := coordinator.Delete(context.Background(), key); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Delete() error = %v, want Unavailable", err)
	}
	if got := deleter.Calls(); len(got) != 0 {
		t.Fatalf("vault calls after failed durable request = %#v", got)
	}
	if got := service.Current(); got.Revision != 0 || len(got.Records) != 0 {
		t.Fatalf("catalog after uncertain request = %#v, want fail-closed empty", got)
	}
	repository.setSaveError(nil)
	reloaded, err := service.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if got := reloaded.Records[0].Status; got != EnrollmentActive {
		t.Fatalf("catalog status after reload = %s, want ACTIVE", got)
	}
}

func TestDeletionCoordinatorRetriesAfterConfirmationFailure(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	key := EnrollmentKey{ProfileRef: "profile-a", Capability: readiness.FaceIdentification}
	coordinatorEnroll(t, service, Registration{
		ProfileRef: key.ProfileRef, Capability: key.Capability,
		TemplateRef: "face-v1", ModelVersion: "face.model.v1",
	})
	deleter := &recordingTemplateDeleter{onDelete: func(Registration) {
		repository.setSaveError(errors.New("catalog offline"))
	}}
	coordinator := newTestDeletionCoordinator(t, service, deleter)

	snapshot, err := coordinator.Delete(context.Background(), key)
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Delete() error = %v, want Unavailable confirmation failure", err)
	}
	if snapshot.Revision != 0 || len(snapshot.Records) != 0 {
		t.Fatalf("confirmation failure snapshot = %#v, want fail-closed empty", snapshot)
	}

	repository.setSaveError(nil)
	deleter.SetOnDelete(nil)
	if _, err := service.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	retried, err := coordinator.RetryPendingDeletes(context.Background())
	if err != nil {
		t.Fatalf("RetryPendingDeletes() error = %v", err)
	}
	if got := retried.Records[0]; got.Status != EnrollmentNone || got.TemplateRef != "" {
		t.Fatalf("retried record = %#v, want confirmed deletion", got)
	}
	if got := deleter.TemplateRefs(); !reflect.DeepEqual(got, []string{"face-v1", "face-v1"}) {
		t.Fatalf("idempotent physical delete calls = %v", got)
	}
}

func TestDeletionCoordinatorRetryFailsClosedWhileCatalogRequiresReload(t *testing.T) {
	repository := &memoryRepository{}
	service := newTestService(t, repository)
	coordinatorEnroll(t, service, Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-v1", ModelVersion: "face.model.v1",
	})
	repository.setSaveError(errors.New("catalog offline"))
	if _, err := service.RevokeConsent(context.Background(), ConsentCommand{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
	}); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("RevokeConsent() error = %v, want Unavailable", err)
	}
	deleter := &recordingTemplateDeleter{}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	if _, err := coordinator.RetryPendingDeletes(context.Background()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("RetryPendingDeletes() error = %v, want Unavailable", err)
	}
	if got := deleter.Calls(); len(got) != 0 {
		t.Fatalf("vault calls while catalog reload is required = %#v", got)
	}
}

func TestDeletionCoordinatorValidatesDependenciesAndContext(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	deleter := &recordingTemplateDeleter{}
	var typedNil *recordingTemplateDeleter
	for _, test := range []struct {
		name    string
		catalog *Service
		deleter TemplateDeleter
	}{
		{name: "nil catalog", deleter: deleter},
		{name: "nil deleter", catalog: service},
		{name: "typed nil deleter", catalog: service, deleter: typedNil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewDeletionCoordinator(test.catalog, test.deleter); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("NewDeletionCoordinator() error = %v, want InvalidInput", err)
			}
		})
	}
	coordinator := newTestDeletionCoordinator(t, service, deleter)
	//lint:ignore SA1012 This boundary must reject a nil caller context.
	if _, err := coordinator.RetryPendingDeletes(nil); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("RetryPendingDeletes(nil) error = %v, want InvalidInput", err)
	}
	if got := deleter.Calls(); len(got) != 0 {
		t.Fatalf("vault calls for nil context = %#v", got)
	}
}

func TestDeletionCoordinatorPreservesCanceledAndDeadlineFaults(t *testing.T) {
	service := newTestService(t, &memoryRepository{})
	deleter := &recordingTemplateDeleter{}
	coordinator := newTestDeletionCoordinator(t, service, deleter)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.RetryPendingDeletes(canceled); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("RetryPendingDeletes(canceled) error = %v, want Unavailable", err)
	}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Time{})
	defer cancelDeadline()
	if _, err := coordinator.RetryPendingDeletes(deadline); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("RetryPendingDeletes(deadline) error = %v, want DeadlineExceeded", err)
	}
	if got := deleter.Calls(); len(got) != 0 {
		t.Fatalf("vault calls for completed contexts = %#v", got)
	}
}

func coordinatorEnroll(t *testing.T, service *Service, registration Registration) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.GrantConsent(ctx, ConsentCommand{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
	}); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
	activateRegistrationForTest(t, service, registration)
}

func newTestDeletionCoordinator(t *testing.T, service *Service, deleter TemplateDeleter) *DeletionCoordinator {
	t.Helper()
	coordinator, err := NewDeletionCoordinator(service, deleter)
	if err != nil {
		t.Fatalf("NewDeletionCoordinator() error = %v", err)
	}
	return coordinator
}

type recordingTemplateDeleter struct {
	mu       sync.Mutex
	calls    []Registration
	failures map[string]error
	onDelete func(Registration)
}

func (d *recordingTemplateDeleter) Delete(_ context.Context, registration Registration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, registration)
	if d.onDelete != nil {
		d.onDelete(registration)
	}
	return d.failures[registration.TemplateRef]
}

func (d *recordingTemplateDeleter) Calls() []Registration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Registration(nil), d.calls...)
}

func (d *recordingTemplateDeleter) TemplateRefs() []string {
	calls := d.Calls()
	refs := make([]string, len(calls))
	for index := range calls {
		refs[index] = calls[index].TemplateRef
	}
	return refs
}

func (d *recordingTemplateDeleter) SetFailures(failures map[string]error) {
	d.mu.Lock()
	d.failures = failures
	d.mu.Unlock()
}

func (d *recordingTemplateDeleter) SetOnDelete(onDelete func(Registration)) {
	d.mu.Lock()
	d.onDelete = onDelete
	d.mu.Unlock()
}
