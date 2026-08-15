package biometricvault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	fileMagic        = "PIEBIOV2"
	templateMagic    = "PIETPLV2"
	maxTemplateBytes = 4 << 20
	privateDirMode   = 0o700
	privateFileMode  = 0o600
)

// MasterKey is the required AES-256 key material. It must be supplied by a
// secret store outside the vault directory.
type MasterKey [32]byte

// MasterKeyProvider loads the device master key without exposing how it is
// stored. An unavailable provider makes Store, Open, and Delete fail closed.
type MasterKeyProvider interface {
	MasterKey(context.Context) (MasterKey, error)
}

// Descriptor is authenticated metadata for one opaque template. None of its
// fields are written into the encrypted file.
type Descriptor struct {
	ProfileRef   string
	Capability   readiness.CapabilityKind
	TemplateRef  string
	ModelVersion string
}

// Vault owns encrypted template files beneath one private absolute directory.
// Instances coordinate through a process mutex and an advisory root lock. All
// trusted same-user processes sharing the root must honor that lock; compromise
// of the operating-system user account is outside this adapter's threat model.
type Vault struct {
	mu     *sync.Mutex
	root   string
	keys   MasterKeyProvider
	random io.Reader
}

var _ biometric.TemplateDeleter = (*Vault)(nil)

// New constructs a vault without creating files or loading the master key.
func New(root string, keys MasterKeyProvider) (*Vault, error) {
	const op = "create biometric vault"
	cleaned, err := validateRoot(root, op)
	if err != nil {
		return nil, err
	}
	if isNil(keys) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("master key provider is required"))
	}
	return &Vault{mu: sharedRootMutex(cleaned), root: cleaned, keys: keys, random: rand.Reader}, nil
}

// store durably creates an encrypted template. It is package-private so every
// production write must pass the fixed-codec and catalog staging coordinator.
func (v *Vault) store(ctx context.Context, descriptor Descriptor, storeOperationID string, template []byte) error {
	const op = "store biometric template"
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	if err := validateDescriptor(op, descriptor); err != nil {
		return err
	}
	if !validStoreOperationID(storeOperationID) {
		return fault.New(fault.InvalidInput, op, errors.New("store operation ID is invalid"))
	}
	if len(template) == 0 || len(template) > maxTemplateBytes {
		return fault.New(fault.InvalidInput, op, errors.New("template must be non-empty and within the size limit"))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	key, err := v.masterKey(ctx, op)
	if err != nil {
		return err
	}
	defer clear(key[:])
	aead, err := newAEAD(key)
	if err != nil {
		return fault.New(fault.PermissionDenied, op, err)
	}
	path, err := templatePath(v.root, descriptor)
	if err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	if err := ensurePrivateDirectory(ctx, v.root, op); err != nil {
		return err
	}
	release, err := acquireVaultLock(v.root, op)
	if err != nil {
		return err
	}
	defer release()
	if err := cleanupOrphanTemps(v.root, ".biometric-", op); err != nil {
		return err
	}
	existing, _, found, err := readSecureFile(path, op)
	if err != nil {
		return err
	}
	if found {
		plain, err := decrypt(aead, descriptor, existing)
		if err != nil {
			return fault.New(fault.AdapterRejected, op, errors.New("existing template authentication failed"))
		}
		defer clear(plain)
		existingOperationID, existingTemplate, err := decodeStoredTemplate(plain)
		if err != nil {
			return fault.New(fault.AdapterRejected, op, errors.New("existing template envelope is invalid"))
		}
		if existingOperationID == storeOperationID && bytes.Equal(existingTemplate, template) {
			return nil
		}
		return fault.New(fault.StaleInput, op, errors.New("template reference belongs to a different store operation or payload"))
	}
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(v.random, nonce); err != nil {
		return fault.New(fault.Unavailable, op, fmt.Errorf("generate encryption nonce: %w", err))
	}
	plain, err := encodeStoredTemplate(storeOperationID, template)
	if err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	defer clear(plain)
	encoded := append([]byte(fileMagic), nonce...)
	encoded = aead.Seal(encoded, nonce, plain, authenticatedMetadata(descriptor))
	if err := createAtomic(ctx, path, encoded, op); err != nil {
		return err
	}
	return nil
}

func validateRoot(root, op string) (string, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return "", fault.New(fault.InvalidInput, op, errors.New("an absolute vault directory is required"))
	}
	cleaned := filepath.Clean(root)
	if cleaned == filepath.VolumeName(cleaned)+string(os.PathSeparator) {
		return "", fault.New(fault.InvalidInput, op, errors.New("filesystem root cannot be used as the vault directory"))
	}
	return cleaned, nil
}

// Open authenticates and decrypts one exact descriptor.
func (v *Vault) Open(ctx context.Context, descriptor Descriptor) ([]byte, error) {
	const op = "open biometric template"
	plain, _, found, err := v.loadIfPresent(ctx, descriptor, op)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fault.New(fault.Unavailable, op, errors.New("template does not exist"))
	}
	return plain, nil
}

// loadIfPresent distinguishes authenticated absence from dependency failure so
// crash recovery never guesses whether a prepared store was published.
func (v *Vault) loadIfPresent(ctx context.Context, descriptor Descriptor, op string) ([]byte, string, bool, error) {
	if err := validateContext(op, ctx); err != nil {
		return nil, "", false, err
	}
	if err := validateDescriptor(op, descriptor); err != nil {
		return nil, "", false, err
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	key, err := v.masterKey(ctx, op)
	if err != nil {
		return nil, "", false, err
	}
	defer clear(key[:])
	aead, err := newAEAD(key)
	if err != nil {
		return nil, "", false, fault.New(fault.PermissionDenied, op, err)
	}
	path, err := templatePath(v.root, descriptor)
	if err != nil {
		return nil, "", false, fault.New(fault.InvalidInput, op, err)
	}
	if err := ensurePrivateDirectory(ctx, v.root, op); err != nil {
		return nil, "", false, err
	}
	release, err := acquireVaultLock(v.root, op)
	if err != nil {
		return nil, "", false, err
	}
	defer release()
	encoded, _, found, err := readSecureFile(path, op)
	if err != nil {
		return nil, "", false, err
	}
	if !found {
		return nil, "", false, nil
	}
	plain, err := decrypt(aead, descriptor, encoded)
	if err != nil {
		return nil, "", false, fault.New(fault.AdapterRejected, op, errors.New("template authentication failed"))
	}
	storeOperationID, template, err := decodeStoredTemplate(plain)
	if err != nil {
		clear(plain)
		return nil, "", false, fault.New(fault.AdapterRejected, op, errors.New("template envelope is invalid"))
	}
	result := append([]byte(nil), template...)
	clear(plain)
	return result, storeOperationID, true, nil
}

// Delete authenticates the complete descriptor before physically removing its
// template. A missing template is idempotently accepted only after the master
// key is available, so an unavailable secret store always fails closed.
func (v *Vault) Delete(ctx context.Context, registration biometric.Registration) error {
	const op = "delete biometric template"
	descriptor := Descriptor{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
		TemplateRef: registration.TemplateRef, ModelVersion: registration.ModelVersion,
	}
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	if err := validateDescriptor(op, descriptor); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	key, err := v.masterKey(ctx, op)
	if err != nil {
		return err
	}
	defer clear(key[:])
	aead, err := newAEAD(key)
	if err != nil {
		return fault.New(fault.PermissionDenied, op, err)
	}
	path, err := templatePath(v.root, descriptor)
	if err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	if err := ensurePrivateDirectory(ctx, v.root, op); err != nil {
		return err
	}
	release, err := acquireVaultLock(v.root, op)
	if err != nil {
		return err
	}
	defer release()
	encoded, identity, found, err := readSecureFile(path, op)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	plain, err := decrypt(aead, descriptor, encoded)
	if err != nil {
		return fault.New(fault.AdapterRejected, op, errors.New("template authentication failed"))
	}
	if _, _, err := decodeStoredTemplate(plain); err != nil {
		clear(plain)
		return fault.New(fault.AdapterRejected, op, errors.New("template envelope is invalid"))
	}
	clear(plain)
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	if err := removeAuthenticated(path, identity, op); err != nil {
		return err
	}
	if err := syncDirectory(v.root); err != nil {
		return filesystemFault(op, err)
	}
	return nil
}

func (v *Vault) masterKey(ctx context.Context, op string) (MasterKey, error) {
	key, err := v.keys.MasterKey(ctx)
	if err != nil {
		return MasterKey{}, classifyProviderFault(op, err)
	}
	var zero MasterKey
	if key == zero {
		return MasterKey{}, fault.New(fault.PermissionDenied, op, errors.New("master key is empty"))
	}
	return key, nil
}

func newAEAD(key MasterKey) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func decrypt(aead cipher.AEAD, descriptor Descriptor, encoded []byte) ([]byte, error) {
	headerSize := len(fileMagic) + aead.NonceSize()
	if len(encoded) < headerSize+aead.Overhead() || string(encoded[:len(fileMagic)]) != fileMagic {
		return nil, errors.New("invalid encrypted template format")
	}
	nonce := encoded[len(fileMagic):headerSize]
	return aead.Open(nil, nonce, encoded[headerSize:], authenticatedMetadata(descriptor))
}

func authenticatedMetadata(descriptor Descriptor) []byte {
	values := []string{
		"biometric-template-v2", descriptor.ProfileRef, string(descriptor.Capability),
		descriptor.TemplateRef, descriptor.ModelVersion,
	}
	size := 0
	for _, value := range values {
		size += 4 + len(value)
	}
	encoded := make([]byte, 0, size)
	for _, value := range values {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		encoded = append(encoded, length[:]...)
		encoded = append(encoded, value...)
	}
	return encoded
}

func encodeStoredTemplate(storeOperationID string, template []byte) ([]byte, error) {
	if !validStoreOperationID(storeOperationID) {
		return nil, errors.New("store operation ID is invalid")
	}
	operation, err := hex.DecodeString(storeOperationID)
	if err != nil || len(operation) != 16 {
		return nil, errors.New("store operation ID is invalid")
	}
	plain := make([]byte, 0, len(templateMagic)+len(operation)+len(template))
	plain = append(plain, templateMagic...)
	plain = append(plain, operation...)
	plain = append(plain, template...)
	return plain, nil
}

func decodeStoredTemplate(plain []byte) (string, []byte, error) {
	const headerBytes = len(templateMagic) + 16
	if len(plain) <= headerBytes || len(plain) > headerBytes+maxTemplateBytes || string(plain[:len(templateMagic)]) != templateMagic {
		return "", nil, errors.New("invalid stored template envelope")
	}
	return hex.EncodeToString(plain[len(templateMagic):headerBytes]), plain[headerBytes:], nil
}

func validStoreOperationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func templatePath(root string, descriptor Descriptor) (string, error) {
	if !validString(descriptor.TemplateRef) {
		return "", errors.New("template reference is required")
	}
	digest := sha256.Sum256([]byte(descriptor.TemplateRef))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".bio"), nil
}

func validateDescriptor(op string, descriptor Descriptor) error {
	if !validString(descriptor.ProfileRef) || !validString(descriptor.TemplateRef) || !validString(descriptor.ModelVersion) {
		return fault.New(fault.InvalidInput, op, errors.New("profile, template, and model references are required without surrounding whitespace"))
	}
	switch descriptor.Capability {
	case readiness.FaceIdentification, readiness.SpeakerIdentification, readiness.SpeakerVerification:
		return nil
	default:
		return fault.New(fault.InvalidInput, op, fmt.Errorf("capability %q does not store profile templates", descriptor.Capability))
	}
}

func validString(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func validateContext(op string, ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fault.New(fault.DeadlineExceeded, op, err)
		}
		return fault.New(fault.Unavailable, op, err)
	}
	return nil
}

func classifyProviderFault(op string, err error) error {
	var typed *fault.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
