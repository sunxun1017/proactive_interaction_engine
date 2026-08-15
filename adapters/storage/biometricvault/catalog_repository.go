package biometricvault

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	catalogFilename = ".catalog.bio"
	catalogSchema   = "v2"
	maxCatalogBytes = 1 << 20
)

var catalogAAD = []byte("proactive-biometric-catalog-v2")

var _ biometric.Repository = (*CatalogRepository)(nil)

// CatalogRepository encrypts per-profile consent, enrollment status, model
// versions, and opaque template references. It never stores template bytes.
type CatalogRepository struct {
	mu     *sync.Mutex
	root   string
	keys   MasterKeyProvider
	random io.Reader
}

// NewCatalogRepository constructs an encrypted biometric metadata repository.
func NewCatalogRepository(root string, keys MasterKeyProvider) (*CatalogRepository, error) {
	const op = "create encrypted biometric catalog"
	cleaned, err := validateRoot(root, op)
	if err != nil {
		return nil, err
	}
	if isNil(keys) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("master key provider is required"))
	}
	return &CatalogRepository{mu: sharedRootMutex(cleaned), root: cleaned, keys: keys, random: rand.Reader}, nil
}

// Load authenticates and decodes the latest catalog snapshot. A missing file
// is revision zero, but the master key must still be available.
func (r *CatalogRepository) Load(ctx context.Context) (biometric.Snapshot, error) {
	const op = "load encrypted biometric catalog"
	if err := validateContext(op, ctx); err != nil {
		return biometric.Snapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	aead, key, err := r.aead(ctx, op)
	if err != nil {
		return biometric.Snapshot{}, err
	}
	defer clear(key[:])
	if err := ensurePrivateDirectory(ctx, r.root, op); err != nil {
		return biometric.Snapshot{}, err
	}
	release, err := acquireVaultLock(r.root, op)
	if err != nil {
		return biometric.Snapshot{}, err
	}
	defer release()
	return r.loadLocked(aead, op)
}

// Save atomically replaces the encrypted catalog when the persisted revision
// matches expectedRevision and snapshot is its immediate successor.
func (r *CatalogRepository) Save(ctx context.Context, expectedRevision uint64, snapshot biometric.Snapshot) error {
	const op = "save encrypted biometric catalog"
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	if expectedRevision == math.MaxUint64 || snapshot.Revision != expectedRevision+1 {
		return fault.New(fault.InvalidInput, op, errors.New("snapshot revision must immediately follow expected revision"))
	}
	plain, err := encodeCatalog(snapshot)
	if err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	defer clear(plain)
	if len(plain) > maxCatalogBytes {
		return fault.New(fault.InvalidInput, op, errors.New("biometric catalog exceeds size limit"))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	aead, key, err := r.aead(ctx, op)
	if err != nil {
		return err
	}
	defer clear(key[:])
	if err := ensurePrivateDirectory(ctx, r.root, op); err != nil {
		return err
	}
	release, err := acquireVaultLock(r.root, op)
	if err != nil {
		return err
	}
	defer release()
	if err := cleanupOrphanTemps(r.root, ".catalog-", op); err != nil {
		return err
	}
	current, err := r.loadLocked(aead, op)
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision {
		return fault.New(
			fault.StaleInput, op,
			fmt.Errorf("persisted revision %d does not match expected revision %d", current.Revision, expectedRevision),
		)
	}
	encoded, err := sealPayload(aead, r.random, catalogAAD, plain)
	if err != nil {
		return fault.New(fault.Unavailable, op, fmt.Errorf("encrypt biometric catalog: %w", err))
	}
	return replaceAtomic(ctx, filepath.Join(r.root, catalogFilename), encoded, op)
}

func (r *CatalogRepository) loadLocked(aead cipher.AEAD, op string) (biometric.Snapshot, error) {
	encoded, _, found, err := readSecureFile(filepath.Join(r.root, catalogFilename), op)
	if err != nil {
		return biometric.Snapshot{}, err
	}
	if !found {
		return biometric.Snapshot{}, nil
	}
	plain, err := openPayload(aead, catalogAAD, encoded)
	if err != nil {
		return biometric.Snapshot{}, fault.New(fault.AdapterRejected, op, errors.New("biometric catalog authentication failed"))
	}
	defer clear(plain)
	if len(plain) > maxCatalogBytes {
		return biometric.Snapshot{}, fault.New(fault.AdapterRejected, op, errors.New("biometric catalog exceeds size limit"))
	}
	snapshot, err := decodeCatalog(plain)
	if err != nil {
		return biometric.Snapshot{}, fault.New(fault.AdapterRejected, op, errors.New("biometric catalog format is invalid"))
	}
	return snapshot, nil
}

func (r *CatalogRepository) aead(ctx context.Context, op string) (cipher.AEAD, MasterKey, error) {
	key, err := r.keys.MasterKey(ctx)
	if err != nil {
		return nil, MasterKey{}, classifyProviderFault(op, err)
	}
	var zero MasterKey
	if key == zero {
		return nil, MasterKey{}, fault.New(fault.PermissionDenied, op, errors.New("master key is empty"))
	}
	aead, err := newAEAD(key)
	if err != nil {
		clear(key[:])
		return nil, MasterKey{}, fault.New(fault.PermissionDenied, op, err)
	}
	return aead, key, nil
}

type catalogDocument struct {
	SchemaVersion string          `json:"schema_version"`
	Revision      uint64          `json:"revision"`
	Records       []catalogRecord `json:"records"`
}

type catalogRecord struct {
	ProfileRef          string                     `json:"profile_ref"`
	Capability          readiness.CapabilityKind   `json:"capability"`
	Consented           bool                       `json:"consented"`
	ConsentVersion      uint64                     `json:"consent_version"`
	ConsentUpdatedAt    time.Time                  `json:"consent_updated_at"`
	TemplateRef         string                     `json:"template_ref,omitempty"`
	ModelVersion        string                     `json:"model_version,omitempty"`
	Status              biometric.EnrollmentStatus `json:"status"`
	EnrollmentUpdatedAt time.Time                  `json:"enrollment_updated_at,omitempty"`
	PendingStore        *catalogPendingTemplate    `json:"pending_store,omitempty"`
	PendingDelete       *catalogTemplateReference  `json:"pending_delete,omitempty"`
}

type catalogPendingTemplate struct {
	TemplateRef      string `json:"template_ref"`
	ModelVersion     string `json:"model_version"`
	ConsentVersion   uint64 `json:"consent_version"`
	StoreOperationID string `json:"store_operation_id"`
}

type catalogTemplateReference struct {
	TemplateRef  string `json:"template_ref"`
	ModelVersion string `json:"model_version"`
}

func encodeCatalog(snapshot biometric.Snapshot) ([]byte, error) {
	document := catalogDocument{
		SchemaVersion: catalogSchema,
		Revision:      snapshot.Revision,
		Records:       make([]catalogRecord, len(snapshot.Records)),
	}
	for index, record := range snapshot.Records {
		document.Records[index] = catalogRecord{
			ProfileRef: record.ProfileRef, Capability: record.Capability,
			Consented: record.Consented, ConsentVersion: record.ConsentVersion, ConsentUpdatedAt: record.ConsentUpdatedAt,
			TemplateRef: record.TemplateRef, ModelVersion: record.ModelVersion,
			Status: record.Status, EnrollmentUpdatedAt: record.EnrollmentUpdatedAt,
		}
		if record.PendingStore != nil {
			document.Records[index].PendingStore = &catalogPendingTemplate{
				TemplateRef: record.PendingStore.TemplateRef, ModelVersion: record.PendingStore.ModelVersion,
				ConsentVersion: record.PendingStore.ConsentVersion, StoreOperationID: record.PendingStore.StoreOperationID,
			}
		}
		if record.PendingDelete != nil {
			document.Records[index].PendingDelete = &catalogTemplateReference{
				TemplateRef: record.PendingDelete.TemplateRef, ModelVersion: record.PendingDelete.ModelVersion,
			}
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode biometric catalog: %w", err)
	}
	return append(encoded, '\n'), nil
}

func decodeCatalog(encoded []byte) (biometric.Snapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var document catalogDocument
	if err := decoder.Decode(&document); err != nil {
		return biometric.Snapshot{}, fmt.Errorf("decode biometric catalog: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return biometric.Snapshot{}, errors.New("multiple biometric catalog documents are not allowed")
	}
	if document.SchemaVersion != catalogSchema {
		return biometric.Snapshot{}, fmt.Errorf("unsupported biometric catalog schema %q", document.SchemaVersion)
	}
	snapshot := biometric.Snapshot{Revision: document.Revision, Records: make([]biometric.Record, len(document.Records))}
	for index, record := range document.Records {
		snapshot.Records[index] = biometric.Record{
			ProfileRef: record.ProfileRef, Capability: record.Capability,
			Consented: record.Consented, ConsentVersion: record.ConsentVersion, ConsentUpdatedAt: record.ConsentUpdatedAt,
			TemplateRef: record.TemplateRef, ModelVersion: record.ModelVersion,
			Status: record.Status, EnrollmentUpdatedAt: record.EnrollmentUpdatedAt,
		}
		if record.PendingStore != nil {
			snapshot.Records[index].PendingStore = &biometric.PendingTemplateReference{
				TemplateRef: record.PendingStore.TemplateRef, ModelVersion: record.PendingStore.ModelVersion,
				ConsentVersion: record.PendingStore.ConsentVersion, StoreOperationID: record.PendingStore.StoreOperationID,
			}
		}
		if record.PendingDelete != nil {
			snapshot.Records[index].PendingDelete = &biometric.TemplateReference{
				TemplateRef: record.PendingDelete.TemplateRef, ModelVersion: record.PendingDelete.ModelVersion,
			}
		}
	}
	return snapshot, nil
}

func sealPayload(aead cipher.AEAD, entropy io.Reader, aad, plain []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return nil, err
	}
	encoded := append([]byte(fileMagic), nonce...)
	return aead.Seal(encoded, nonce, plain, aad), nil
}

func openPayload(aead cipher.AEAD, aad, encoded []byte) ([]byte, error) {
	headerSize := len(fileMagic) + aead.NonceSize()
	if len(encoded) < headerSize+aead.Overhead() || string(encoded[:len(fileMagic)]) != fileMagic {
		return nil, errors.New("invalid encrypted payload format")
	}
	nonce := encoded[len(fileMagic):headerSize]
	return aead.Open(nil, nonce, encoded[headerSize:], aad)
}

func replaceAtomic(ctx context.Context, path string, encoded []byte, op string) error {
	parent := filepath.Dir(path)
	if _, _, _, err := readSecureFile(path, op); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".catalog-*.tmp")
	if err != nil {
		return filesystemFault(op, err)
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(privateFileMode); err != nil {
		return filesystemFault(op, err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return filesystemFault(op, err)
	}
	if err := temporary.Sync(); err != nil {
		return filesystemFault(op, err)
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return filesystemFault(op, err)
	}
	closed = true
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	if err := rejectUnsafeDirectoryIfPresent(parent, op); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fault.New(fault.PermissionDenied, op, errors.New("catalog file must not be a symlink"))
		}
		return filesystemFault(op, err)
	}
	if err := syncDirectory(parent); err != nil {
		return filesystemFault(op, err)
	}
	return nil
}
