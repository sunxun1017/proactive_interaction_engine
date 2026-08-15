package biometricvault

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

const (
	testLifecycleOperationID  = "11111111111111111111111111111111"
	wrongLifecycleOperationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestTemplateLifecycleRecoveryCommitsPublishedStoreAndAbortsAbsentStore(t *testing.T) {
	for _, test := range []struct {
		name          string
		publish       bool
		wantStatus    biometric.EnrollmentStatus
		wantReference string
	}{
		{name: "prepared only", wantStatus: biometric.EnrollmentNone},
		{name: "published store", publish: true, wantStatus: biometric.EnrollmentActive, wantReference: "face-template"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "vault")
			keys := staticKeyProvider{key: testKey(71)}
			repository, err := NewCatalogRepository(root, keys)
			if err != nil {
				t.Fatal(err)
			}
			catalog := newLifecycleCatalog(t, repository)
			registration := faceRegistration("face-template")
			grantLifecycleConsent(t, catalog, registration)
			if _, err := catalog.PrepareRegister(ctx, registration, testLifecycleOperationID); err != nil {
				t.Fatalf("PrepareRegister() error = %v", err)
			}
			vault := newLifecycleVault(t, root, keys)
			if test.publish {
				if err := vault.store(ctx, descriptorForRegistration(registration), testLifecycleOperationID, validFaceTemplatePayload()); err != nil {
					t.Fatalf("store() error = %v", err)
				}
			}

			restartedRepository, err := NewCatalogRepository(root, keys)
			if err != nil {
				t.Fatal(err)
			}
			restarted := newLifecycleCatalog(t, restartedRepository)
			coordinator := newLifecycleCoordinator(t, restarted, vault)
			recovered, err := coordinator.Recover(ctx)
			if err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			record := recovered.Records[0]
			if record.Status != test.wantStatus || record.TemplateRef != test.wantReference || record.PendingStore != nil {
				t.Fatalf("recovered record = %#v", record)
			}
		})
	}
}

func TestTemplateLifecycleCommitAfterPersistedSaveErrorReloadsWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	repository := &lifecycleRepository{failAfterPersistAt: 3}
	catalog := newLifecycleCatalog(t, repository)
	registration := faceRegistration("face-template")
	grantLifecycleConsent(t, catalog, registration)
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(72)})
	coordinator := newLifecycleCoordinator(t, catalog, vault)

	if _, err := coordinator.Register(ctx, registration, validFaceTemplatePayload()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Register() error = %v, want Unavailable", err)
	}
	if current := catalog.Current(); current.Revision != 0 || len(current.Records) != 0 {
		t.Fatalf("catalog after uncertain commit = %#v, want fail-closed empty", current)
	}
	if opened, err := vault.Open(ctx, descriptorForRegistration(registration)); err != nil || len(opened) != faceTemplateBytes {
		t.Fatalf("stored template after uncertain commit bytes = %d, error = %v", len(opened), err)
	}

	repository.clearFailure()
	recovered, err := coordinator.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if got := recovered.Records[0]; got.Status != biometric.EnrollmentActive || got.TemplateRef != registration.TemplateRef || got.PendingStore != nil {
		t.Fatalf("recovered uncertain commit = %#v", got)
	}
}

func TestTemplateLifecycleRecoveryDeletesStoreAfterRevokeAndRegrant(t *testing.T) {
	ctx := context.Background()
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	registration := speakerRegistration(readiness.SpeakerVerification, "speaker-template")
	grantLifecycleConsent(t, catalog, registration)
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(73)})
	if _, err := catalog.PrepareRegister(ctx, registration, testLifecycleOperationID); err != nil {
		t.Fatal(err)
	}
	if err := vault.store(ctx, descriptorForRegistration(registration), testLifecycleOperationID, validSpeakerTemplatePayload()); err != nil {
		t.Fatal(err)
	}
	command := biometric.ConsentCommand{ProfileRef: registration.ProfileRef, Capability: registration.Capability}
	if _, err := catalog.RevokeConsent(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.GrantConsent(ctx, command); err != nil {
		t.Fatal(err)
	}

	recovered, err := newLifecycleCoordinator(t, catalog, vault).Recover(ctx)
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if got := recovered.Records[0]; got.Status != biometric.EnrollmentNone || got.PendingStore != nil || !got.Consented {
		t.Fatalf("recovered revoked store = %#v", got)
	}
	if _, err := vault.Open(ctx, descriptorForRegistration(registration)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(deleted staged template) error = %v, want Unavailable", err)
	}
}

func TestTemplateLifecycleRejectsInvalidCodecBeforeCatalogOrVaultMutation(t *testing.T) {
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	registration := faceRegistration("face-template")
	grantLifecycleConsent(t, catalog, registration)
	before := catalog.Current()
	root := filepath.Join(t.TempDir(), "vault")
	vault := newLifecycleVault(t, root, staticKeyProvider{key: testKey(74)})
	coordinator := newLifecycleCoordinator(t, catalog, vault)

	if _, err := coordinator.Register(context.Background(), registration, []byte("raw-media")); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Register(raw media) error = %v, want InvalidInput", err)
	}
	after := catalog.Current()
	if after.Revision != before.Revision || after.Records[0].PendingStore != nil {
		t.Fatalf("invalid codec changed catalog: before=%#v after=%#v", before, after)
	}
	if _, err := vault.Open(context.Background(), descriptorForRegistration(registration)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(invalid store) error = %v, want Unavailable", err)
	}
}

func TestTemplateLifecycleSerializesExactConcurrentRegistration(t *testing.T) {
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	registration := speakerRegistration(readiness.SpeakerIdentification, "speaker-template")
	grantLifecycleConsent(t, catalog, registration)
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(75)})
	coordinator := newLifecycleCoordinator(t, catalog, vault)
	payload := validSpeakerTemplatePayload()

	const callers = 8
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := coordinator.Register(context.Background(), registration, payload)
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Register() error = %v", err)
		}
	}
	if got := catalog.Current(); got.Revision != 3 || got.Records[0].Status != biometric.EnrollmentActive {
		t.Fatalf("concurrent registration snapshot = %#v", got)
	}
}

func TestTemplateLifecycleRecoveryRejectsAndConvergesWrongStoreOperation(t *testing.T) {
	ctx := context.Background()
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	registration := faceRegistration("face-template")
	grantLifecycleConsent(t, catalog, registration)
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(76)})
	oldPayload := validFaceTemplatePayload()
	newPayload := validFaceTemplatePayloadAt(1)
	if err := vault.store(ctx, descriptorForRegistration(registration), wrongLifecycleOperationID, oldPayload); err != nil {
		t.Fatalf("store(wrong operation) error = %v", err)
	}
	coordinator := newLifecycleCoordinator(t, catalog, vault)

	if _, err := coordinator.Register(ctx, registration, newPayload); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Register(conflicting store) error = %v, want StaleInput", err)
	}
	prepared := catalog.Current().Records[0]
	if prepared.PendingStore == nil || prepared.PendingStore.StoreOperationID != testLifecycleOperationID {
		t.Fatalf("prepared operation = %#v", prepared.PendingStore)
	}

	if _, err := coordinator.Recover(ctx); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Recover(wrong operation) error = %v, want StaleInput", err)
	}
	after := catalog.Current().Records[0]
	if after.Status != biometric.EnrollmentNone || after.PendingStore != nil || after.TemplateRef != "" {
		t.Fatalf("catalog after wrong-operation recovery = %#v", after)
	}
	if _, err := vault.Open(ctx, descriptorForRegistration(registration)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(wrong-operation store) error = %v, want Unavailable", err)
	}

	registered, err := coordinator.Register(ctx, registration, newPayload)
	if err != nil {
		t.Fatalf("Register(after convergence) error = %v", err)
	}
	if got := registered.Records[0]; got.Status != biometric.EnrollmentActive || got.TemplateRef != registration.TemplateRef || got.PendingStore != nil {
		t.Fatalf("registered after convergence = %#v", got)
	}
}

func TestTemplateLifecycleRecoveryContinuesCanonicalRecordsAfterInvalidCodec(t *testing.T) {
	ctx := context.Background()
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	face := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template", ModelVersion: FaceInferenceProfileID,
	}
	speaker := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification,
		TemplateRef: "speaker-template", ModelVersion: SpeakerInferenceProfileID,
	}
	for _, registration := range []biometric.Registration{speaker, face} {
		grantLifecycleConsent(t, catalog, registration)
		if _, err := catalog.PrepareRegister(ctx, registration, testLifecycleOperationID); err != nil {
			t.Fatalf("PrepareRegister(%s) error = %v", registration.Capability, err)
		}
	}
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(77)})
	if err := vault.store(ctx, descriptorForRegistration(face), testLifecycleOperationID, validSpeakerTemplatePayload()); err != nil {
		t.Fatalf("store(invalid face codec) error = %v", err)
	}
	if err := vault.store(ctx, descriptorForRegistration(speaker), testLifecycleOperationID, validSpeakerTemplatePayload()); err != nil {
		t.Fatalf("store(valid speaker codec) error = %v", err)
	}

	recovered, err := newLifecycleCoordinator(t, catalog, vault).Recover(ctx)
	if !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Recover() error = %v, want first AdapterRejected", err)
	}
	if len(recovered.Records) != 2 {
		t.Fatalf("Recover() records = %#v", recovered.Records)
	}
	if got := recovered.Records[0]; got.Capability != readiness.FaceIdentification || got.Status != biometric.EnrollmentNone || got.PendingStore != nil {
		t.Fatalf("first canonical record = %#v, want cleaned invalid face", got)
	}
	if got := recovered.Records[1]; got.Capability != readiness.SpeakerIdentification || got.Status != biometric.EnrollmentActive || got.PendingStore != nil {
		t.Fatalf("second canonical record = %#v, want committed speaker", got)
	}
}

func TestTemplateLifecycleDeleteProfileContinuesStagedCleanupInCanonicalOrder(t *testing.T) {
	ctx := context.Background()
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	face := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "face-template", ModelVersion: FaceInferenceProfileID,
	}
	speaker := biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.SpeakerIdentification,
		TemplateRef: "speaker-template", ModelVersion: SpeakerInferenceProfileID,
	}
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(78)})
	for _, staged := range []struct {
		registration biometric.Registration
		payload      []byte
	}{
		{registration: speaker, payload: validSpeakerTemplatePayload()},
		{registration: face, payload: validFaceTemplatePayload()},
	} {
		grantLifecycleConsent(t, catalog, staged.registration)
		if _, err := catalog.PrepareRegister(ctx, staged.registration, testLifecycleOperationID); err != nil {
			t.Fatal(err)
		}
		if err := vault.store(ctx, descriptorForRegistration(staged.registration), testLifecycleOperationID, staged.payload); err != nil {
			t.Fatal(err)
		}
	}
	facePath := onlyExpectedPath(t, vault.root, descriptorForRegistration(face))
	tampered, err := os.ReadFile(facePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(facePath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := newLifecycleCoordinator(t, catalog, vault).DeleteProfile(ctx, "profile-a")
	if !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("DeleteProfile() error = %v, want first AdapterRejected", err)
	}
	if len(snapshot.Records) != 2 {
		t.Fatalf("DeleteProfile() records = %#v", snapshot.Records)
	}
	if got := snapshot.Records[0]; got.Capability != readiness.FaceIdentification || got.Consented || got.PendingStore == nil {
		t.Fatalf("failed first staged cleanup record = %#v", got)
	}
	if got := snapshot.Records[1]; got.Capability != readiness.SpeakerIdentification || got.Consented || got.PendingStore != nil {
		t.Fatalf("continued second staged cleanup record = %#v", got)
	}
	if _, err := vault.Open(ctx, descriptorForRegistration(speaker)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open(cleaned speaker store) error = %v, want Unavailable", err)
	}
}

func TestTemplateLifecycleEntropyFailureDoesNotPrepareCatalog(t *testing.T) {
	repository := &lifecycleRepository{}
	catalog := newLifecycleCatalog(t, repository)
	registration := faceRegistration("face-template")
	grantLifecycleConsent(t, catalog, registration)
	vault := newLifecycleVault(t, filepath.Join(t.TempDir(), "vault"), staticKeyProvider{key: testKey(79)})
	coordinator, err := newTemplateLifecycleCoordinator(catalog, vault, failingReader{err: errors.New("entropy unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	before := catalog.Current()
	if _, err := coordinator.Register(context.Background(), registration, validFaceTemplatePayload()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Register() error = %v, want Unavailable", err)
	}
	after := catalog.Current()
	if after.Revision != before.Revision || after.Records[0].PendingStore != nil {
		t.Fatalf("entropy failure changed catalog: before=%#v after=%#v", before, after)
	}
}

func newLifecycleCoordinator(t *testing.T, catalog *biometric.Service, vault *Vault) *TemplateLifecycleCoordinator {
	t.Helper()
	entropy, err := hex.DecodeString(testLifecycleOperationID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := newTemplateLifecycleCoordinator(catalog, vault, bytes.NewReader(bytes.Repeat(entropy, 64)))
	if err != nil {
		t.Fatalf("NewTemplateLifecycleCoordinator() error = %v", err)
	}
	return coordinator
}

func newLifecycleCatalog(t *testing.T, repository biometric.Repository) *biometric.Service {
	t.Helper()
	catalog, err := biometric.New(
		context.Background(), repository,
		engineclock.NewFake(time.Date(2026, 8, 15, 18, 0, 0, 0, time.UTC)),
	)
	if err != nil {
		t.Fatalf("biometric.New() error = %v", err)
	}
	return catalog
}

func newLifecycleVault(t *testing.T, root string, keys MasterKeyProvider) *Vault {
	t.Helper()
	vault, err := New(root, keys)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return vault
}

func grantLifecycleConsent(t *testing.T, catalog *biometric.Service, registration biometric.Registration) {
	t.Helper()
	if _, err := catalog.GrantConsent(context.Background(), biometric.ConsentCommand{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
	}); err != nil {
		t.Fatalf("GrantConsent() error = %v", err)
	}
}

func faceRegistration(templateRef string) biometric.Registration {
	return biometric.Registration{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: templateRef, ModelVersion: FaceInferenceProfileID,
	}
}

func speakerRegistration(capability readiness.CapabilityKind, templateRef string) biometric.Registration {
	return biometric.Registration{
		ProfileRef: "profile-a", Capability: capability,
		TemplateRef: templateRef, ModelVersion: SpeakerInferenceProfileID,
	}
}

type lifecycleRepository struct {
	mu                 sync.Mutex
	current            biometric.Snapshot
	saveCalls          int
	failAfterPersistAt int
}

func (r *lifecycleRepository) Load(context.Context) (biometric.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneLifecycleSnapshot(r.current), nil
}

func (r *lifecycleRepository) Save(_ context.Context, expected uint64, snapshot biometric.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current.Revision != expected {
		return fault.New(fault.StaleInput, "test repository", errors.New("revision conflict"))
	}
	r.saveCalls++
	r.current = cloneLifecycleSnapshot(snapshot)
	if r.saveCalls == r.failAfterPersistAt {
		return errors.New("directory sync failed after rename")
	}
	return nil
}

func (r *lifecycleRepository) clearFailure() {
	r.mu.Lock()
	r.failAfterPersistAt = 0
	r.mu.Unlock()
}

func cloneLifecycleSnapshot(snapshot biometric.Snapshot) biometric.Snapshot {
	snapshot.Records = append([]biometric.Record(nil), snapshot.Records...)
	for index := range snapshot.Records {
		if snapshot.Records[index].PendingStore != nil {
			pending := *snapshot.Records[index].PendingStore
			snapshot.Records[index].PendingStore = &pending
		}
		if snapshot.Records[index].PendingDelete != nil {
			pending := *snapshot.Records[index].PendingDelete
			snapshot.Records[index].PendingDelete = &pending
		}
	}
	return snapshot
}
