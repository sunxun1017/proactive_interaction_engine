package biometricvault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestVaultEncryptsTemplateAndBindsMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(1))
	descriptor := testDescriptor()
	template := []byte("sensitive-biometric-template")

	if err := vault.Store(context.Background(), descriptor, template); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	path := onlyVaultPath(t, root)
	if filepath.Base(path) == descriptor.TemplateRef {
		t.Fatalf("vault template path = %q, want opaque name", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat() error = %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("vault file mode = %v", info.Mode())
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, secret := range [][]byte{
		template,
		[]byte(descriptor.ProfileRef),
		[]byte(descriptor.TemplateRef),
		[]byte(descriptor.ModelVersion),
	} {
		if bytes.Contains(encoded, secret) {
			t.Fatalf("encrypted file exposes %q", secret)
		}
	}

	opened, err := vault.Open(context.Background(), descriptor)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !bytes.Equal(opened, template) {
		t.Fatalf("Open() = %q, want %q", opened, template)
	}
	opened[0] ^= 0xff
	again, err := vault.Open(context.Background(), descriptor)
	if err != nil || !bytes.Equal(again, template) {
		t.Fatalf("second Open() = %q, %v", again, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Descriptor)
	}{
		{name: "profile", mutate: func(value *Descriptor) { value.ProfileRef = "profile-b" }},
		{name: "capability", mutate: func(value *Descriptor) { value.Capability = readiness.SpeakerIdentification }},
		{name: "model", mutate: func(value *Descriptor) { value.ModelVersion = "face.v2" }},
	} {
		t.Run("reject substituted "+test.name, func(t *testing.T) {
			substituted := descriptor
			test.mutate(&substituted)
			if _, err := vault.Open(context.Background(), substituted); !fault.IsCode(err, fault.AdapterRejected) {
				t.Fatalf("Open(substituted metadata) error = %v, want AdapterRejected", err)
			}
		})
	}
}

func TestVaultStoreIsIdempotentButNeverOverwritesDifferentTemplate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(2))
	descriptor := testDescriptor()
	template := []byte("template-v1")
	ctx := context.Background()

	if err := vault.Store(ctx, descriptor, template); err != nil {
		t.Fatalf("Store(first) error = %v", err)
	}
	before := readOnlyVaultFile(t, root)
	if err := vault.Store(ctx, descriptor, append([]byte(nil), template...)); err != nil {
		t.Fatalf("Store(idempotent) error = %v", err)
	}
	if after := readOnlyVaultFile(t, root); !bytes.Equal(after, before) {
		t.Fatal("idempotent Store() rewrote ciphertext")
	}
	if err := vault.Store(ctx, descriptor, []byte("different")); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Store(different template) error = %v, want StaleInput", err)
	}
	if opened, err := vault.Open(ctx, descriptor); err != nil || !bytes.Equal(opened, template) {
		t.Fatalf("Open() after rejected overwrite = %q, %v", opened, err)
	}
}

func TestVaultRejectsTamperAndWrongKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	descriptor := testDescriptor()
	vault := newTestVault(t, root, testKey(3))
	if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	wrongKey := newTestVault(t, root, testKey(4))
	if _, err := wrongKey.Open(context.Background(), descriptor); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Open(wrong key) error = %v, want AdapterRejected", err)
	}

	path := onlyVaultPath(t, root)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Open(tampered) error = %v, want AdapterRejected", err)
	}
}

func TestVaultRejectsCiphertextCopiedUnderAnotherTemplateReference(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	descriptor := testDescriptor()
	vault := newTestVault(t, root, testKey(10))
	if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	encoded := readOnlyVaultFile(t, root)
	substituted := descriptor
	substituted.TemplateRef = "template-b"
	if err := os.WriteFile(onlyExpectedPath(t, root, substituted), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Open(context.Background(), substituted); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Open(copied ciphertext) error = %v, want AdapterRejected", err)
	}
}

func TestVaultFailsClosedWhenMasterKeyIsUnavailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault, err := New(root, failingKeyProvider{err: errors.New("keyring locked")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := vault.Store(context.Background(), testDescriptor(), []byte("template")); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Store() error = %v, want Unavailable", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault directory after failed key lookup error = %v, want not exist", err)
	}
	if _, err := vault.Open(context.Background(), testDescriptor()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open() error = %v, want Unavailable", err)
	}
}

func TestVaultDeleteAuthenticatesDescriptorAndIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	descriptor := testDescriptor()
	vault := newTestVault(t, root, testKey(5))
	if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	withoutKey, err := New(root, failingKeyProvider{err: errors.New("keyring unavailable")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := withoutKey.Delete(context.Background(), deletionRegistration(descriptor)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Delete(without key) error = %v, want Unavailable", err)
	}
	wrong := descriptor
	wrong.ProfileRef = "profile-b"
	if err := vault.Delete(context.Background(), deletionRegistration(wrong)); !fault.IsCode(err, fault.AdapterRejected) {
		t.Fatalf("Delete(wrong descriptor) error = %v, want AdapterRejected", err)
	}
	if err := vault.Delete(context.Background(), deletionRegistration(descriptor)); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := vault.Delete(context.Background(), deletionRegistration(descriptor)); err != nil {
		t.Fatalf("Delete(idempotent) error = %v", err)
	}
	if _, err := os.Stat(onlyExpectedPath(t, root, descriptor)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted template error = %v, want not exist", err)
	}
}

func TestVaultRejectsUnsafePathsAndTemplateFiles(t *testing.T) {
	t.Run("existing vault directory is not repaired", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		vault := newTestVault(t, root, testKey(11))
		if err := vault.Store(context.Background(), testDescriptor(), []byte("template")); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Store() error = %v, want PermissionDenied", err)
		}
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("vault directory mode = %v, want unchanged 0755", info.Mode())
		}
	})

	t.Run("vault directory symlink", func(t *testing.T) {
		parent := t.TempDir()
		private := filepath.Join(parent, "private")
		if err := os.Mkdir(private, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(parent, "vault-link")
		if err := os.Symlink(private, root); err != nil {
			t.Fatal(err)
		}
		descriptor := testDescriptor()
		target := onlyExpectedPath(t, private, descriptor)
		if err := os.WriteFile(target, []byte("must-survive"), 0o600); err != nil {
			t.Fatal(err)
		}
		vault := newTestVault(t, root, testKey(6))
		if err := vault.Store(context.Background(), descriptor, []byte("template")); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Store() error = %v, want PermissionDenied", err)
		}
		if _, err := vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Open() error = %v, want PermissionDenied", err)
		}
		if err := vault.Delete(context.Background(), deletionRegistration(descriptor)); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Delete() error = %v, want PermissionDenied", err)
		}
		if contents, err := os.ReadFile(target); err != nil || string(contents) != "must-survive" {
			t.Fatalf("symlink target = %q, %v", contents, err)
		}
	})

	t.Run("template file symlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		descriptor := testDescriptor()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("do-not-touch"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, onlyExpectedPath(t, root, descriptor)); err != nil {
			t.Fatal(err)
		}
		vault := newTestVault(t, root, testKey(7))
		if _, err := vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Open() error = %v, want PermissionDenied", err)
		}
		if err := vault.Delete(context.Background(), deletionRegistration(descriptor)); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Delete() error = %v, want PermissionDenied", err)
		}
		contents, err := os.ReadFile(target)
		if err != nil || string(contents) != "do-not-touch" {
			t.Fatalf("symlink target = %q, %v", contents, err)
		}
	})

	t.Run("template file hard link", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		vault := newTestVault(t, root, testKey(12))
		descriptor := testDescriptor()
		if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
			t.Fatal(err)
		}
		path := onlyVaultPath(t, root)
		if err := os.Link(path, filepath.Join(root, "unexpected-hard-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := vault.Open(context.Background(), descriptor); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Open() error = %v, want PermissionDenied", err)
		}
		if err := vault.Delete(context.Background(), deletionRegistration(descriptor)); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Delete() error = %v, want PermissionDenied", err)
		}
	})
}

func TestVaultCleansCrashOrphanBeforeIdempotentRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(13))
	descriptor := testDescriptor()
	if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, ".biometric-crash.tmp")
	if err := os.Link(onlyVaultPath(t, root), orphan); err != nil {
		t.Fatal(err)
	}
	if err := vault.Store(context.Background(), descriptor, []byte("template")); err != nil {
		t.Fatalf("Store(recover) error = %v", err)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan temporary file error = %v, want not exist", err)
	}
	if opened, err := vault.Open(context.Background(), descriptor); err != nil || string(opened) != "template" {
		t.Fatalf("Open() after recovery = %q, %v", opened, err)
	}
}

func TestVaultValidatesInputAndContextBeforeFilesystemAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(8))
	invalid := testDescriptor()
	invalid.ProfileRef = " profile-a"
	if err := vault.Store(context.Background(), invalid, []byte("template")); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Store(invalid descriptor) error = %v, want InvalidInput", err)
	}
	if err := vault.Store(context.Background(), testDescriptor(), nil); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Store(empty template) error = %v, want InvalidInput", err)
	}
	if err := vault.Store(context.Background(), testDescriptor(), make([]byte, maxTemplateBytes+1)); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Store(oversized template) error = %v, want InvalidInput", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := vault.Store(cancelled, testDescriptor(), []byte("template")); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Store(cancelled) error = %v, want Unavailable", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault directory after invalid calls error = %v, want not exist", err)
	}
}

func TestVaultEntropyFailureNeverPublishesTemplate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(14))
	vault.random = failingReader{err: errors.New("entropy unavailable")}
	if err := vault.Store(context.Background(), testDescriptor(), []byte("template")); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Store() error = %v, want Unavailable", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".bio" {
			t.Fatalf("published template after entropy failure: %q", entry.Name())
		}
	}
}

func TestVaultSerializesConcurrentIdempotentStores(t *testing.T) {
	vault := newTestVault(t, filepath.Join(t.TempDir(), "vault"), testKey(9))
	const writers = 16
	errorsSeen := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- vault.Store(context.Background(), testDescriptor(), []byte("same-template"))
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Store() error = %v", err)
		}
	}
}

func TestVaultFailsFastWhenRootLockIsOwnedElsewhere(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	if err := ensurePrivateDirectory(context.Background(), root, "test vault lock"); err != nil {
		t.Fatal(err)
	}
	release, err := acquireVaultLock(root, "test vault lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	vault := newTestVault(t, root, testKey(15))
	if _, err := vault.Open(context.Background(), testDescriptor()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Open() error = %v, want Unavailable", err)
	}
}

func TestAuthenticatedRemovalRejectsReplacedInode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vault")
	vault := newTestVault(t, root, testKey(16))
	descriptor := testDescriptor()
	if err := vault.Store(context.Background(), descriptor, []byte("original")); err != nil {
		t.Fatal(err)
	}
	path := onlyVaultPath(t, root)
	_, identity, found, err := readSecureFile(path, "test authenticated removal")
	if err != nil || !found {
		t.Fatalf("readSecureFile() found = %v, error = %v", found, err)
	}
	replacement := filepath.Join(root, ".replacement")
	if err := os.WriteFile(replacement, []byte("replacement-must-survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := removeAuthenticated(path, identity, "test authenticated removal"); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("removeAuthenticated() error = %v, want StaleInput", err)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "replacement-must-survive" {
		t.Fatalf("replacement contents = %q, %v", contents, err)
	}
}

type staticKeyProvider struct{ key MasterKey }

func (p staticKeyProvider) MasterKey(context.Context) (MasterKey, error) { return p.key, nil }

type failingKeyProvider struct{ err error }

func (p failingKeyProvider) MasterKey(context.Context) (MasterKey, error) {
	return MasterKey{}, p.err
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func newTestVault(t *testing.T, root string, key MasterKey) *Vault {
	t.Helper()
	vault, err := New(root, staticKeyProvider{key: key})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return vault
}

func testKey(seed byte) MasterKey {
	var key MasterKey
	for index := range key {
		key[index] = seed + byte(index)
	}
	return key
}

func testDescriptor() Descriptor {
	return Descriptor{
		ProfileRef: "profile-a", Capability: readiness.FaceIdentification,
		TemplateRef: "template-a", ModelVersion: "face.v1",
	}
}

func deletionRegistration(descriptor Descriptor) biometric.Registration {
	return biometric.Registration{
		ProfileRef: descriptor.ProfileRef, Capability: descriptor.Capability,
		TemplateRef: descriptor.TemplateRef, ModelVersion: descriptor.ModelVersion,
	}
}

func onlyVaultPath(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var templates []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".bio" && entry.Name() != catalogFilename {
			templates = append(templates, filepath.Join(root, entry.Name()))
		}
	}
	if len(templates) != 1 {
		t.Fatalf("template files = %v, want 1; all entries = %#v", templates, entries)
	}
	return templates[0]
}

func onlyExpectedPath(t *testing.T, root string, descriptor Descriptor) string {
	t.Helper()
	path, err := templatePath(root, descriptor)
	if err != nil {
		t.Fatalf("templatePath() error = %v", err)
	}
	return path
}

func readOnlyVaultFile(t *testing.T, root string) []byte {
	t.Helper()
	contents, err := os.ReadFile(onlyVaultPath(t, root))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
