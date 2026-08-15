package biometricvault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestCatalogRepositoryRoundTripsOnlyEncryptedMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	repository := newTestCatalogRepository(t, root, testKey(21))
	ctx := context.Background()
	if initial, err := repository.Load(ctx); err != nil || !reflect.DeepEqual(initial, biometric.Snapshot{}) {
		t.Fatalf("Load(initial) = %#v, %v", initial, err)
	}
	want := testCatalogSnapshot()
	if err := repository.Save(ctx, 0, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path := filepath.Join(root, catalogFilename)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("catalog mode = %v", info.Mode())
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"profile-a", "template-a", "template-b", "face.v1", string(readiness.FaceIdentification)} {
		if bytes.Contains(encoded, []byte(sensitive)) {
			t.Fatalf("encrypted catalog exposes %q", sensitive)
		}
	}

	loaded, err := repository.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("Load() = %#v, want %#v", loaded, want)
	}
	loaded.Records[0].TemplateRef = "mutated"
	again, err := repository.Load(ctx)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("Load() after caller mutation = %#v, %v", again, err)
	}
}

func TestCatalogRepositoryRejectsV1AuthenticationDomain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	key := testKey(29)
	if err := ensurePrivateDirectory(context.Background(), root, "test v1 catalog"); err != nil {
		t.Fatal(err)
	}
	aead, err := newAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealPayload(
		aead, bytes.NewReader(make([]byte, aead.NonceSize())),
		[]byte("proactive-biometric-catalog-v1"),
		[]byte(`{"schema_version":"v1","revision":0,"records":[]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, catalogFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	repository := newTestCatalogRepository(t, root, key)
	if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Load(v1 AAD) error = %v, want AdapterRejected", err)
	}
}

func TestCatalogRepositoryEnforcesImmediateOptimisticRevision(t *testing.T) {
	repository := newTestCatalogRepository(t, filepath.Join(t.TempDir(), "vault"), testKey(22))
	ctx := context.Background()
	first := testCatalogSnapshot()
	if err := repository.Save(ctx, 0, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Revision = 2
	second.Records = append(second.Records, biometric.Record{
		ProfileRef: "profile-b", Capability: readiness.SpeakerIdentification,
		Consented: true, ConsentVersion: 1, ConsentUpdatedAt: testCatalogTime().Add(time.Minute),
		Status: biometric.EnrollmentNone,
	})
	if err := repository.Save(ctx, 0, first); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Save(stale expected revision) error = %v, want StaleInput", err)
	}
	if err := repository.Save(ctx, 1, first); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Save(non-successor revision) error = %v, want InvalidInput", err)
	}
	if loaded, err := repository.Load(ctx); err != nil || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("Load() after rejected saves = %#v, %v", loaded, err)
	}
	if err := repository.Save(ctx, 1, second); err != nil {
		t.Fatalf("Save(second) error = %v", err)
	}
}

func TestCatalogRepositoryRejectsWrongKeyAndTamper(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	repository := newTestCatalogRepository(t, root, testKey(23))
	if err := repository.Save(context.Background(), 0, testCatalogSnapshot()); err != nil {
		t.Fatal(err)
	}
	wrongKey := newTestCatalogRepository(t, root, testKey(24))
	if _, err := wrongKey.Load(context.Background()); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Load(wrong key) error = %v, want AdapterRejected", err)
	}

	path := filepath.Join(root, catalogFilename)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Load(tampered) error = %v, want AdapterRejected", err)
	}
}

func TestCatalogAndTemplateCiphertextsCannotCrossAuthenticationDomains(t *testing.T) {
	t.Run("catalog ciphertext as template", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		key := testKey(26)
		repository := newTestCatalogRepository(t, root, key)
		if err := repository.Save(context.Background(), 0, testCatalogSnapshot()); err != nil {
			t.Fatal(err)
		}
		encoded, err := os.ReadFile(filepath.Join(root, catalogFilename))
		if err != nil {
			t.Fatal(err)
		}
		descriptor := testDescriptor()
		if err := os.WriteFile(onlyExpectedPath(t, root, descriptor), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		vault := newTestVault(t, root, key)
		if _, err := vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.AdapterRejected) {
			t.Fatalf("Open(catalog ciphertext) error = %v, want AdapterRejected", err)
		}
	})

	t.Run("template ciphertext as catalog", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		key := testKey(27)
		vault := newTestVault(t, root, key)
		if err := vault.store(context.Background(), testDescriptor(), testVaultStoreOperationID, []byte("template")); err != nil {
			t.Fatal(err)
		}
		encoded := readOnlyVaultFile(t, root)
		if err := os.WriteFile(filepath.Join(root, catalogFilename), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		repository := newTestCatalogRepository(t, root, key)
		if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.AdapterRejected) {
			t.Fatalf("Load(template ciphertext) error = %v, want AdapterRejected", err)
		}
	})
}

func TestCatalogRepositoryRejectsUnknownSchemaFieldsAndTrailingDocuments(t *testing.T) {
	for _, test := range []struct {
		name  string
		plain string
	}{
		{name: "old schema", plain: `{"schema_version":"v1","revision":0,"records":[]}`},
		{name: "unknown field", plain: `{"schema_version":"v2","revision":0,"records":[],"extra":true}`},
		{name: "trailing document", plain: `{"schema_version":"v2","revision":0,"records":[]} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "vault")
			key := testKey(28)
			writeEncryptedCatalog(t, root, key, []byte(test.plain))
			repository := newTestCatalogRepository(t, root, key)
			if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.AdapterRejected) {
				t.Fatalf("Load() error = %v, want AdapterRejected", err)
			}
		})
	}
}

func TestCatalogRepositoryFailsClosedWithoutMasterKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	repository, err := NewCatalogRepository(root, failingKeyProvider{err: errors.New("keyring locked")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Load() error = %v, want Unavailable", err)
	}
	if err := repository.Save(context.Background(), 0, testCatalogSnapshot()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Save() error = %v, want Unavailable", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("catalog directory after failed key lookup error = %v, want not exist", err)
	}
}

func TestCatalogRepositorySerializesCompetingRevisions(t *testing.T) {
	repository := newTestCatalogRepository(t, filepath.Join(t.TempDir(), "vault"), testKey(25))
	first := testCatalogSnapshot()
	if err := repository.Save(context.Background(), 0, first); err != nil {
		t.Fatal(err)
	}
	next := first
	next.Revision = 2
	const writers = 12
	results := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- repository.Save(context.Background(), 1, next)
		}()
	}
	wait.Wait()
	close(results)
	succeeded := 0
	stale := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case fault.IsCode(err, fault.StaleInput):
			stale++
		default:
			t.Fatalf("Save() error = %v", err)
		}
	}
	if succeeded != 1 || stale != writers-1 {
		t.Fatalf("competing saves = %d success, %d stale", succeeded, stale)
	}
}

func newTestCatalogRepository(t *testing.T, root string, key MasterKey) *CatalogRepository {
	t.Helper()
	repository, err := NewCatalogRepository(root, staticKeyProvider{key: key})
	if err != nil {
		t.Fatalf("NewCatalogRepository() error = %v", err)
	}
	return repository
}

func testCatalogSnapshot() biometric.Snapshot {
	return biometric.Snapshot{
		Revision: 1,
		Records: []biometric.Record{{
			ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
			Consented: true, ConsentVersion: 1, ConsentUpdatedAt: testCatalogTime(),
			TemplateRef: "template-a", ModelVersion: "face.v1",
			Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: testCatalogTime(),
			PendingStore: &biometric.PendingTemplateReference{
				TemplateRef: "template-b", ModelVersion: "face.v2", ConsentVersion: 1,
				StoreOperationID: "00112233445566778899aabbccddeeff",
			},
		}},
	}
}

func testCatalogTime() time.Time {
	return time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
}

func writeEncryptedCatalog(t *testing.T, root string, key MasterKey, plain []byte) {
	t.Helper()
	if err := ensurePrivateDirectory(context.Background(), root, "test catalog fixture"); err != nil {
		t.Fatal(err)
	}
	aead, err := newAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := sealPayload(aead, bytes.NewReader(make([]byte, aead.NonceSize())), catalogAAD, plain)
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireVaultLock(root, "test catalog fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := replaceAtomic(context.Background(), filepath.Join(root, catalogFilename), encoded, "test catalog fixture"); err != nil {
		t.Fatal(err)
	}
}
