package biometricvault

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestSecretServiceMasterKeyProviderRejectsInvalidConstruction(t *testing.T) {
	t.Parallel()
	valid := SecretServiceMasterKeyConfig{VaultRoot: filepath.Join(t.TempDir(), "vault"), RuntimeDirectory: t.TempDir()}
	factory := &fakeSecretStoreFactory{store: &fakeSecretStore{}}

	tests := []struct {
		name   string
		config SecretServiceMasterKeyConfig
		stores secretServiceStoreFactory
		random io.Reader
	}{
		{name: "relative vault", config: SecretServiceMasterKeyConfig{VaultRoot: "relative/vault", RuntimeDirectory: t.TempDir()}, stores: factory, random: bytes.NewReader(make([]byte, 32))},
		{name: "relative runtime", config: SecretServiceMasterKeyConfig{VaultRoot: filepath.Join(t.TempDir(), "vault"), RuntimeDirectory: "runtime"}, stores: factory, random: bytes.NewReader(make([]byte, 32))},
		{name: "nil factory", config: valid, stores: nil, random: bytes.NewReader(make([]byte, 32))},
		{name: "typed nil factory", config: valid, stores: (*fakeSecretStoreFactory)(nil), random: bytes.NewReader(make([]byte, 32))},
		{name: "typed nil entropy", config: valid, stores: factory, random: (*bytes.Reader)(nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newSecretServiceMasterKeyProvider(test.config, test.stores, test.random)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("newSecretServiceMasterKeyProvider() error = %v, want InvalidInput", err)
			}
		})
	}
	if _, err := (&SecretServiceMasterKeyProvider{}).MasterKey(context.Background()); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("zero provider MasterKey() error = %v, want InvalidInput", err)
	}
}

func TestSecretServiceMasterKeyProviderCreatesVerifiesAndReusesKey(t *testing.T) {
	t.Parallel()
	store := &fakeSecretStore{}
	factory := &fakeSecretStoreFactory{store: store}
	entropy := &countingEntropy{value: 37}
	provider := newTestSecretServiceProvider(t, factory, entropy)

	first, err := provider.MasterKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.MasterKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || isZeroMasterKey(first) {
		t.Fatalf("MasterKey() returned inconsistent or zero keys")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.createCalls != 1 || store.lookupCalls != 3 {
		t.Fatalf("calls = create %d, lookup %d; want 1, 3", store.createCalls, store.lookupCalls)
	}
	if entropy.read != len(MasterKey{}) {
		t.Fatalf("entropy bytes read = %d, want %d", entropy.read, len(MasterKey{}))
	}
}

func TestSecretServiceMasterKeyProviderSerializesConcurrentCreation(t *testing.T) {
	store := &fakeSecretStore{}
	provider := newTestSecretServiceProvider(t, &fakeSecretStoreFactory{store: store}, &countingEntropy{value: 51})

	const callers = 16
	keys := make(chan MasterKey, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			key, err := provider.MasterKey(context.Background())
			keys <- key
			errs <- err
		}()
	}
	group.Wait()
	close(keys)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("MasterKey() error = %v", err)
		}
	}
	var expected MasterKey
	for key := range keys {
		if isZeroMasterKey(expected) {
			expected = key
		}
		if key != expected {
			t.Fatal("concurrent MasterKey() calls returned different keys")
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.createCalls != 1 {
		t.Fatalf("Create() calls = %d, want 1", store.createCalls)
	}
}

func TestSecretServiceMasterKeyProviderNeverRegeneratesWithProtectedData(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{catalogFilename, string(bytes.Repeat([]byte{'a'}, 64)) + ".bio"} {
		t.Run(filename, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, filename), []byte("protected"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := &fakeSecretStore{}
			entropy := &countingEntropy{value: 73}
			provider := newTestSecretServiceProviderAt(t, root, t.TempDir(), &fakeSecretStoreFactory{store: store}, entropy)
			_, err := provider.MasterKey(context.Background())
			if !fault.IsCode(err, fault.AdapterRejected) {
				t.Fatalf("MasterKey() error = %v, want AdapterRejected", err)
			}
			if entropy.read != 0 || store.createCallCount() != 0 {
				t.Fatalf("missing-key regeneration attempted: entropy=%d create=%d", entropy.read, store.createCallCount())
			}
		})
	}
}

func TestSecretServiceMasterKeyProviderIgnoresNonfinalVaultArtifacts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, filename := range []string{".biometric-orphan.tmp", vaultLockFilename, "not-a-template.bio"} {
		if err := os.WriteFile(filepath.Join(root, filename), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	provider := newTestSecretServiceProviderAt(t, root, t.TempDir(), &fakeSecretStoreFactory{store: &fakeSecretStore{}}, &countingEntropy{value: 91})
	if _, err := provider.MasterKey(context.Background()); err != nil {
		t.Fatalf("MasterKey() error = %v", err)
	}
}

func TestSecretServiceMasterKeyProviderFailsClosedForUnsafeProtectedData(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, catalogFilename)); err != nil {
		t.Fatal(err)
	}
	provider := newTestSecretServiceProviderAt(t, root, t.TempDir(), &fakeSecretStoreFactory{store: &fakeSecretStore{}}, &countingEntropy{value: 99})
	if _, err := provider.MasterKey(context.Background()); !fault.IsCode(err, fault.PermissionDenied) {
		t.Fatalf("MasterKey() error = %v, want PermissionDenied", err)
	}
}

func TestSecretServiceMasterKeyProviderMapsContextAndBackendFaults(t *testing.T) {
	t.Parallel()
	backendFault := fault.New(fault.PermissionDenied, "fake store", errors.New("locked"))
	tests := []struct {
		name    string
		ctx     func() context.Context
		factory *fakeSecretStoreFactory
		want    fault.Code
	}{
		{name: "canceled", ctx: canceledContext, factory: &fakeSecretStoreFactory{store: &fakeSecretStore{}}, want: fault.Unavailable},
		{name: "deadline", ctx: expiredContext, factory: &fakeSecretStoreFactory{store: &fakeSecretStore{}}, want: fault.DeadlineExceeded},
		{name: "open unavailable", ctx: context.Background, factory: &fakeSecretStoreFactory{err: errors.New("no bus")}, want: fault.Unavailable},
		{name: "typed fault preserved", ctx: context.Background, factory: &fakeSecretStoreFactory{store: &fakeSecretStore{lookupErr: backendFault}}, want: fault.PermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestSecretServiceProvider(t, test.factory, &countingEntropy{value: 1})
			_, err := provider.MasterKey(test.ctx())
			if !fault.IsCode(err, test.want) {
				t.Fatalf("MasterKey() error = %v, want %s", err, test.want)
			}
		})
	}
}

func TestSecretServiceMasterKeyProviderCancellationWhileQueued(t *testing.T) {
	t.Parallel()
	provider := newTestSecretServiceProvider(t, &fakeSecretStoreFactory{store: &fakeSecretStore{}}, &countingEntropy{value: 4})
	<-provider.gate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := provider.MasterKey(ctx)
	provider.gate <- struct{}{}
	if !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("MasterKey() error = %v, want Unavailable", err)
	}
}

func TestSecretServiceMasterKeyProviderRejectsInvalidOrUnverifiedKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store *fakeSecretStore
		want  fault.Code
	}{
		{name: "existing zero", store: &fakeSecretStore{found: true}, want: fault.AdapterRejected},
		{name: "create mismatch", store: &fakeSecretStore{createdOverride: keyPointer(testKey(99))}, want: fault.AdapterRejected},
		{name: "create missing", store: &fakeSecretStore{discardCreate: true}, want: fault.AdapterRejected},
		{name: "create locked", store: &fakeSecretStore{createErr: fault.New(fault.PermissionDenied, "fake", errors.New("locked"))}, want: fault.PermissionDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestSecretServiceProvider(t, &fakeSecretStoreFactory{store: test.store}, &countingEntropy{value: 8})
			_, err := provider.MasterKey(context.Background())
			if !fault.IsCode(err, test.want) {
				t.Fatalf("MasterKey() error = %v, want %s", err, test.want)
			}
		})
	}
}

func TestSecretServiceMasterKeyProviderUsesCrossProcessLock(t *testing.T) {
	runtimeDirectory := t.TempDir()
	release, err := acquirePrivateFileLock(runtimeDirectory, masterKeyLockFilename, "test master key", "test")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeSecretStore{}
	provider := newTestSecretServiceProviderAt(t, filepath.Join(t.TempDir(), "vault"), runtimeDirectory, &fakeSecretStoreFactory{store: store}, &countingEntropy{value: 12})
	if _, err := provider.MasterKey(context.Background()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("MasterKey() while locked error = %v, want Unavailable", err)
	}
	release()
	if _, err := provider.MasterKey(context.Background()); err != nil {
		t.Fatalf("MasterKey() after release error = %v", err)
	}
}

func newTestSecretServiceProvider(t *testing.T, stores secretServiceStoreFactory, random io.Reader) *SecretServiceMasterKeyProvider {
	t.Helper()
	return newTestSecretServiceProviderAt(t, filepath.Join(t.TempDir(), "vault"), t.TempDir(), stores, random)
}

func newTestSecretServiceProviderAt(t *testing.T, root, runtimeDirectory string, stores secretServiceStoreFactory, random io.Reader) *SecretServiceMasterKeyProvider {
	t.Helper()
	if err := os.Chmod(runtimeDirectory, privateDirMode); err != nil {
		t.Fatalf("chmod runtime directory: %v", err)
	}
	if _, err := os.Stat(root); err == nil {
		if err := os.Chmod(root, privateDirMode); err != nil {
			t.Fatalf("chmod vault root: %v", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat vault root: %v", err)
	}
	provider, err := newSecretServiceMasterKeyProvider(SecretServiceMasterKeyConfig{
		VaultRoot: root, RuntimeDirectory: runtimeDirectory,
	}, stores, random)
	if err != nil {
		t.Fatalf("newSecretServiceMasterKeyProvider() error = %v", err)
	}
	return provider
}

type fakeSecretStoreFactory struct {
	mu    sync.Mutex
	store *fakeSecretStore
	err   error
	opens int
}

func (f *fakeSecretStoreFactory) Open(context.Context) (secretServiceStore, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.err != nil {
		return nil, f.err
	}
	return f.store, nil
}

type fakeSecretStore struct {
	mu              sync.Mutex
	key             MasterKey
	found           bool
	lookupErr       error
	createErr       error
	createdOverride *MasterKey
	discardCreate   bool
	lookupCalls     int
	createCalls     int
}

func (s *fakeSecretStore) Lookup(context.Context) (MasterKey, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupCalls++
	return s.key, s.found, s.lookupErr
}

func (s *fakeSecretStore) Create(_ context.Context, key MasterKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return s.createErr
	}
	if s.discardCreate {
		return nil
	}
	if s.createdOverride != nil {
		s.key = *s.createdOverride
	} else {
		s.key = key
	}
	s.found = true
	return nil
}

func (*fakeSecretStore) Close() error { return nil }

func (s *fakeSecretStore) createCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCalls
}

type countingEntropy struct {
	mu    sync.Mutex
	value byte
	read  int
}

func (r *countingEntropy) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range buffer {
		buffer[index] = r.value + byte(index)
	}
	r.read += len(buffer)
	return len(buffer), nil
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	cancel()
	return ctx
}

func keyPointer(key MasterKey) *MasterKey { return &key }
