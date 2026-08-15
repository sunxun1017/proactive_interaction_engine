package biometricvault

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"proactive-interaction-engine/internal/domain/fault"
)

const masterKeyLockFilename = ".biometric-master-key.lock"

// SecretServiceMasterKeyConfig identifies the protected vault and the private
// runtime directory used only for cross-process coordination. Neither path is
// used to persist key material.
type SecretServiceMasterKeyConfig struct {
	VaultRoot        string
	RuntimeDirectory string
}

// SecretServiceMasterKeyProvider loads the biometric vault key from the
// desktop Secret Service. It deliberately keeps no in-process key cache.
type SecretServiceMasterKeyProvider struct {
	vaultRoot        string
	runtimeDirectory string
	stores           secretServiceStoreFactory
	random           io.Reader
	gate             chan struct{}
}

type secretServiceStoreFactory interface {
	Open(context.Context) (secretServiceStore, error)
}

type secretServiceStore interface {
	Lookup(context.Context) (MasterKey, bool, error)
	Create(context.Context, MasterKey) error
	Close() error
}

var _ MasterKeyProvider = (*SecretServiceMasterKeyProvider)(nil)

// NewSecretServiceMasterKeyProvider constructs the production Secret Service
// provider. It does not contact D-Bus or create directories until MasterKey is
// called.
func NewSecretServiceMasterKeyProvider(config SecretServiceMasterKeyConfig) (*SecretServiceMasterKeyProvider, error) {
	return newSecretServiceMasterKeyProvider(config, dbusSecretServiceFactory{random: rand.Reader}, rand.Reader)
}

func newSecretServiceMasterKeyProvider(config SecretServiceMasterKeyConfig, stores secretServiceStoreFactory, random io.Reader) (*SecretServiceMasterKeyProvider, error) {
	const op = "create Secret Service master key provider"
	vaultRoot, err := validateRoot(config.VaultRoot, op)
	if err != nil {
		return nil, err
	}
	runtimeDirectory, err := validateRoot(config.RuntimeDirectory, op)
	if err != nil {
		return nil, err
	}
	if isNil(stores) || isNil(random) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("Secret Service factory and entropy source are required"))
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &SecretServiceMasterKeyProvider{
		vaultRoot: vaultRoot, runtimeDirectory: runtimeDirectory,
		stores: stores, random: random, gate: gate,
	}, nil
}

// MasterKey returns the existing 256-bit key or creates it only when no
// finalized protected vault data exists. Creation is serialized across
// goroutines and cooperating processes without writing the key to disk.
func (p *SecretServiceMasterKeyProvider) MasterKey(ctx context.Context) (MasterKey, error) {
	const op = "load Secret Service biometric master key"
	if ctx == nil {
		return MasterKey{}, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if p == nil || p.gate == nil || isNil(p.stores) || isNil(p.random) || p.vaultRoot == "" || p.runtimeDirectory == "" {
		return MasterKey{}, fault.New(fault.InvalidInput, op, errors.New("Secret Service master key provider is not initialized"))
	}
	if err := validateContext(op, ctx); err != nil {
		return MasterKey{}, err
	}
	select {
	case <-p.gate:
		defer func() { p.gate <- struct{}{} }()
	case <-ctx.Done():
		return MasterKey{}, contextFault(op, ctx.Err())
	}
	if err := ensurePrivateDirectory(ctx, p.runtimeDirectory, op); err != nil {
		return MasterKey{}, err
	}
	release, err := acquirePrivateFileLock(p.runtimeDirectory, masterKeyLockFilename, "biometric master key creation", op)
	if err != nil {
		return MasterKey{}, err
	}
	defer release()

	store, err := p.stores.Open(ctx)
	if err != nil {
		return MasterKey{}, classifySecretServiceFault(op, err)
	}
	if isNil(store) {
		return MasterKey{}, fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned no store"))
	}
	defer store.Close()

	key, found, err := store.Lookup(ctx)
	if err != nil {
		return MasterKey{}, classifySecretServiceFault(op, err)
	}
	if found {
		return checkedMasterKey(op, key)
	}
	protected, err := protectedVaultDataExists(ctx, p.vaultRoot, op)
	if err != nil {
		return MasterKey{}, err
	}
	if protected {
		return MasterKey{}, fault.New(fault.AdapterRejected, op, errors.New("Secret Service key is missing while finalized protected vault data exists"))
	}
	if err := validateContext(op, ctx); err != nil {
		return MasterKey{}, err
	}

	var generated MasterKey
	if _, err := io.ReadFull(p.random, generated[:]); err != nil {
		return MasterKey{}, fault.New(fault.Unavailable, op, fmt.Errorf("generate master key: %w", err))
	}
	defer clear(generated[:])
	if isZeroMasterKey(generated) {
		return MasterKey{}, fault.New(fault.AdapterRejected, op, errors.New("entropy source returned an invalid zero key"))
	}
	if err := store.Create(ctx, generated); err != nil {
		return MasterKey{}, classifySecretServiceFault(op, err)
	}
	created, found, err := store.Lookup(ctx)
	if err != nil {
		return MasterKey{}, classifySecretServiceFault(op, err)
	}
	if !found || isZeroMasterKey(created) || subtle.ConstantTimeCompare(created[:], generated[:]) != 1 {
		clear(created[:])
		return MasterKey{}, fault.New(fault.AdapterRejected, op, errors.New("Secret Service did not return the newly created key"))
	}
	return created, nil
}

func checkedMasterKey(op string, key MasterKey) (MasterKey, error) {
	if isZeroMasterKey(key) {
		return MasterKey{}, fault.New(fault.AdapterRejected, op, errors.New("Secret Service returned an invalid zero key"))
	}
	return key, nil
}

func isZeroMasterKey(key MasterKey) bool {
	var zero MasterKey
	return subtle.ConstantTimeCompare(key[:], zero[:]) == 1
}

func protectedVaultDataExists(ctx context.Context, root, op string) (bool, error) {
	if err := rejectUnsafeDirectoryIfPresent(root, op); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, filesystemFault(op, err)
	}
	for _, entry := range entries {
		if err := validateContext(op, ctx); err != nil {
			return false, err
		}
		if !isFinalProtectedFilename(entry.Name()) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return false, filesystemFault(op, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Nlink != 1 {
			return false, fault.New(fault.PermissionDenied, op, errors.New("finalized protected vault file is unsafe"))
		}
		return true, nil
	}
	return false, nil
}

func isFinalProtectedFilename(name string) bool {
	if name == catalogFilename {
		return true
	}
	if len(name) != 64+len(".bio") || !strings.HasSuffix(name, ".bio") {
		return false
	}
	digest := strings.TrimSuffix(name, ".bio")
	if strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == 32
}

func contextFault(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}
