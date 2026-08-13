package privacy

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestNewDefaultsEveryPermissionToDisabled(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC))
	repository := &recordingRepository{}
	service, err := New(context.Background(), repository, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	snapshot, err := service.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if snapshot.Revision != 0 || len(snapshot.Grants) != len(AllPermissions()) {
		t.Fatalf("Current() = %#v, want revision zero and every permission", snapshot)
	}
	for index, permission := range AllPermissions() {
		grant := snapshot.Grants[index]
		if grant.Permission != permission || grant.Enabled || !grant.UpdatedAt.IsZero() {
			t.Fatalf("grant[%d] = %#v, want disabled %s", index, grant, permission)
		}
	}
}

func TestChangePersistsOneGrantAndReturnsDefensiveSnapshots(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	repository := &recordingRepository{}
	service, err := New(context.Background(), repository, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	changed, err := service.Change(context.Background(), ChangePermission{
		Permission: MicrophoneCapture,
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("Change() error = %v", err)
	}
	if changed.Revision != 1 {
		t.Fatalf("Change().Revision = %d, want 1", changed.Revision)
	}
	assertGrant(t, changed, MicrophoneCapture, true, now)
	if repository.saveCount() != 1 {
		t.Fatalf("repository saves = %d, want 1", repository.saveCount())
	}
	expectedRevision, saved := repository.lastSave()
	if expectedRevision != 0 || !reflect.DeepEqual(saved, changed) {
		t.Fatalf("Save() = expected %d snapshot %#v, want 0 and %#v", expectedRevision, saved, changed)
	}

	changed.Grants[0].Enabled = true
	current, err := service.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	assertGrant(t, current, CameraCapture, false, time.Time{})
	assertGrant(t, current, MicrophoneCapture, true, now)
	current.Grants = nil
	again, err := service.Current(context.Background())
	if err != nil || len(again.Grants) != len(AllPermissions()) {
		t.Fatalf("second Current() = %#v, %v, want defensive full snapshot", again, err)
	}
}

func TestChangeIsDesiredStateIdempotent(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC))
	repository := &recordingRepository{}
	service, err := New(context.Background(), repository, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := service.Change(context.Background(), ChangePermission{Permission: CameraCapture, Enabled: true})
	if err != nil {
		t.Fatalf("first Change() error = %v", err)
	}
	clock.Advance(time.Minute)
	second, err := service.Change(context.Background(), ChangePermission{Permission: CameraCapture, Enabled: true})
	if err != nil {
		t.Fatalf("second Change() error = %v", err)
	}
	if !reflect.DeepEqual(second, first) || repository.saveCount() != 1 {
		t.Fatalf("idempotent Change() = %#v with %d saves, want %#v with 1 save", second, repository.saveCount(), first)
	}
}

func TestSaveFailureDoesNotPublishPermission(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC))
	repository := &recordingRepository{saveErr: errors.New("disk full")}
	service, err := New(context.Background(), repository, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = service.Change(context.Background(), ChangePermission{Permission: FaceIdentification, Enabled: true})
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Change() error = %v, want Unavailable", err)
	}
	current, currentErr := service.Current(context.Background())
	if currentErr != nil {
		t.Fatalf("Current() error = %v", currentErr)
	}
	if current.Revision != 0 {
		t.Fatalf("Current().Revision = %d, want rollback to 0", current.Revision)
	}
	assertGrant(t, current, FaceIdentification, false, time.Time{})
}

func TestRepositoryFaultClassificationIsPreserved(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC))
	repository := &recordingRepository{
		saveErr: fault.New(fault.StaleInput, "save privacy permissions", errors.New("revision changed")),
	}
	service, err := New(context.Background(), repository, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = service.Change(context.Background(), ChangePermission{Permission: CameraCapture, Enabled: true})
	if !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Change() error = %v, want repository StaleInput preserved", err)
	}
}

func TestNewRestoresAndCanonicalizesPersistedSnapshot(t *testing.T) {
	updated := time.Date(2026, time.August, 13, 18, 30, 0, 0, time.UTC)
	repository := &recordingRepository{loaded: Snapshot{
		Revision: 7,
		Grants: []Grant{
			{Permission: SpeakerIdentification, Enabled: true, UpdatedAt: updated},
			{Permission: CameraCapture, Enabled: true, UpdatedAt: updated},
		},
	}}
	service, err := New(context.Background(), repository, engineclock.NewFake(updated.Add(time.Hour)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	snapshot, err := service.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if snapshot.Revision != 7 || len(snapshot.Grants) != len(AllPermissions()) {
		t.Fatalf("Current() = %#v, want canonical restored snapshot", snapshot)
	}
	assertGrant(t, snapshot, CameraCapture, true, updated)
	assertGrant(t, snapshot, SpeakerIdentification, true, updated)
	assertGrant(t, snapshot, MicrophoneCapture, false, time.Time{})
}

func TestInvalidDependenciesSnapshotsAndCommandsFailClosed(t *testing.T) {
	now := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	var nilRepository *recordingRepository
	var nilClock *engineclock.Fake
	for _, test := range []struct {
		name       string
		repository Repository
		clock      port.Clock
	}{
		{name: "nil repository", clock: clock},
		{name: "typed nil repository", repository: nilRepository, clock: clock},
		{name: "nil clock", repository: &recordingRepository{}},
		{name: "typed nil clock", repository: &recordingRepository{}, clock: nilClock},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, err := New(context.Background(), test.repository, test.clock)
			if service != nil || !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("New() = %#v, %v, want nil and InvalidInput", service, err)
			}
		})
	}

	for _, loaded := range []Snapshot{
		{Revision: 1, Grants: []Grant{{Permission: Permission("UNKNOWN")}}},
		{Revision: 1, Grants: []Grant{{Permission: CameraCapture}, {Permission: CameraCapture}}},
		{Revision: 0, Grants: []Grant{{Permission: CameraCapture, Enabled: true, UpdatedAt: now}}},
		{Revision: 1, Grants: []Grant{{Permission: CameraCapture, Enabled: true}}},
	} {
		service, err := New(context.Background(), &recordingRepository{loaded: loaded}, clock)
		if service != nil || !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("New(loaded=%#v) = %#v, %v, want nil and InvalidInput", loaded, service, err)
		}
	}

	service, err := New(context.Background(), &recordingRepository{}, clock)
	if err != nil {
		t.Fatalf("New(valid) error = %v", err)
	}
	for _, command := range []ChangePermission{
		{},
		{Permission: Permission("UNKNOWN"), Enabled: true},
	} {
		if _, err := service.Change(context.Background(), command); !fault.IsCode(err, fault.InvalidInput) {
			t.Fatalf("Change(%#v) error = %v, want InvalidInput", command, err)
		}
	}
}

func TestConcurrentReadsAndChangesAreRaceSafe(t *testing.T) {
	clock := engineclock.NewFake(time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC))
	service, err := New(context.Background(), &recordingRepository{}, clock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const workers = 16
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range workers {
		index := index
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			_, _ = service.Current(context.Background())
		}()
		go func() {
			defer group.Done()
			<-start
			permissions := AllPermissions()
			_, _ = service.Change(context.Background(), ChangePermission{
				Permission: permissions[index%len(permissions)],
				Enabled:    index%2 == 0,
			})
		}()
	}
	close(start)
	group.Wait()
}

func assertGrant(t *testing.T, snapshot Snapshot, permission Permission, enabled bool, updatedAt time.Time) {
	t.Helper()
	for _, grant := range snapshot.Grants {
		if grant.Permission == permission {
			if grant.Enabled != enabled || grant.UpdatedAt != updatedAt {
				t.Fatalf("grant %s = %#v, want enabled=%t updatedAt=%s", permission, grant, enabled, updatedAt)
			}
			return
		}
	}
	t.Fatalf("snapshot %#v has no grant for %s", snapshot, permission)
}

type recordingRepository struct {
	mu      sync.Mutex
	loaded  Snapshot
	loadErr error
	saves   []savedSnapshot
	saveErr error
}

type savedSnapshot struct {
	expectedRevision uint64
	snapshot         Snapshot
}

func (r *recordingRepository) Load(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSnapshot(r.loaded), r.loadErr
}

func (r *recordingRepository) Save(ctx context.Context, expectedRevision uint64, snapshot Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, savedSnapshot{expectedRevision: expectedRevision, snapshot: cloneSnapshot(snapshot)})
	return r.saveErr
}

func (r *recordingRepository) saveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.saves)
}

func (r *recordingRepository) lastSave() (uint64, Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.saves[len(r.saves)-1]
	return last.expectedRevision, cloneSnapshot(last.snapshot)
}
