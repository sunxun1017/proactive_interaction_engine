package privacyfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	schemaVersion    = "v1"
	maxDocumentBytes = 64 << 10
	privateDirMode   = 0o700
	privateFileMode  = 0o600
)

// Repository stores one revisioned privacy snapshot at an explicit path.
type Repository struct {
	mu   sync.Mutex
	path string
}

type document struct {
	SchemaVersion string          `json:"schema_version"`
	Revision      uint64          `json:"revision"`
	Grants        []grantDocument `json:"grants"`
}

type grantDocument struct {
	Permission privacy.Permission `json:"permission"`
	Enabled    bool               `json:"enabled"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// New constructs a repository for an explicit absolute file path.
func New(path string) (*Repository, error) {
	const op = "create privacy file repository"
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("an absolute path is required"))
	}
	cleaned := filepath.Clean(path)
	if cleaned == filepath.Dir(cleaned) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("path must identify a file"))
	}
	return &Repository{path: cleaned}, nil
}

// Load reads and strictly decodes the current snapshot. A missing file is the
// safe never-configured state at revision zero.
func (r *Repository) Load(ctx context.Context) (privacy.Snapshot, error) {
	const op = "load privacy permissions"
	if err := validateContext(op, ctx); err != nil {
		return privacy.Snapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked(ctx, op)
}

// Save atomically replaces the snapshot only when the persisted revision
// equals expectedRevision and snapshot is its immediate successor.
func (r *Repository) Save(ctx context.Context, expectedRevision uint64, snapshot privacy.Snapshot) error {
	const op = "save privacy permissions"
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	encoded, err := encodeDocument(snapshot)
	if err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	if len(encoded) > maxDocumentBytes {
		return fault.New(fault.InvalidInput, op, errors.New("privacy document exceeds size limit"))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	current, err := r.loadLocked(ctx, op)
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision {
		return fault.New(
			fault.StaleInput,
			op,
			fmt.Errorf("persisted revision %d does not match expected revision %d", current.Revision, expectedRevision),
		)
	}
	if expectedRevision == math.MaxUint64 || snapshot.Revision != expectedRevision+1 {
		return fault.New(fault.InvalidInput, op, errors.New("snapshot revision must immediately follow expected revision"))
	}
	if err := validateContext(op, ctx); err != nil {
		return err
	}
	return r.replaceLocked(ctx, encoded, op)
}

func (r *Repository) loadLocked(ctx context.Context, op string) (privacy.Snapshot, error) {
	if err := validateContext(op, ctx); err != nil {
		return privacy.Snapshot{}, err
	}
	info, err := os.Lstat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return privacy.Snapshot{}, nil
	}
	if err != nil {
		return privacy.Snapshot{}, filesystemFault(op, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return privacy.Snapshot{}, fault.New(fault.PermissionDenied, op, errors.New("privacy file must not be a symlink"))
	}
	if !info.Mode().IsRegular() {
		return privacy.Snapshot{}, fault.New(fault.InvalidInput, op, errors.New("privacy path is not a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return privacy.Snapshot{}, fault.New(fault.PermissionDenied, op, errors.New("privacy file is accessible to group or world"))
	}

	file, err := os.OpenFile(r.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return privacy.Snapshot{}, fault.New(fault.PermissionDenied, op, errors.New("privacy file must not be a symlink"))
		}
		return privacy.Snapshot{}, filesystemFault(op, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return privacy.Snapshot{}, filesystemFault(op, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return privacy.Snapshot{}, fault.New(fault.PermissionDenied, op, errors.New("privacy file changed during secure open"))
	}

	encoded, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return privacy.Snapshot{}, filesystemFault(op, err)
	}
	if len(encoded) > maxDocumentBytes {
		return privacy.Snapshot{}, fault.New(fault.InvalidInput, op, errors.New("privacy document exceeds size limit"))
	}
	if err := validateContext(op, ctx); err != nil {
		return privacy.Snapshot{}, err
	}
	decoded, err := decodeDocument(encoded)
	if err != nil {
		return privacy.Snapshot{}, fault.New(fault.InvalidInput, op, err)
	}
	return decoded, nil
}

func (r *Repository) replaceLocked(ctx context.Context, encoded []byte, op string) error {
	parent := filepath.Dir(r.path)
	if err := ensurePrivateDirectory(ctx, parent, op); err != nil {
		return err
	}
	if err := rejectUnsafeExistingTarget(r.path, op); err != nil {
		return err
	}
	if err := validateContext(op, ctx); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(parent, ".privacy-*.tmp")
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
	if err := os.Rename(temporaryPath, r.path); err != nil {
		return filesystemFault(op, err)
	}
	if err := syncDirectory(parent); err != nil {
		return filesystemFault(op, err)
	}
	return nil
}

func ensurePrivateDirectory(ctx context.Context, directory, op string) error {
	cleaned := filepath.Clean(directory)
	volume := filepath.VolumeName(cleaned)
	root := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(cleaned, root)
	current := root
	for _, element := range strings.Split(relative, string(os.PathSeparator)) {
		if element == "" || element == "." {
			continue
		}
		if err := validateContext(op, ctx); err != nil {
			return err
		}
		current = filepath.Join(current, element)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, privateDirMode); err != nil && !errors.Is(err, os.ErrExist) {
				return filesystemFault(op, err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return filesystemFault(op, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fault.New(fault.PermissionDenied, op, fmt.Errorf("directory %s must not be a symlink", current))
		}
		if !info.IsDir() {
			return fault.New(fault.InvalidInput, op, fmt.Errorf("path component %s is not a directory", current))
		}
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return filesystemFault(op, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if cleaned == root || info.Mode()&os.ModeSticky != 0 {
			return fault.New(fault.PermissionDenied, op, errors.New("shared directory cannot be used as the privacy directory"))
		}
		if err := os.Chmod(cleaned, privateDirMode); err != nil {
			return filesystemFault(op, err)
		}
		info, err = os.Lstat(cleaned)
		if err != nil {
			return filesystemFault(op, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return fault.New(fault.PermissionDenied, op, errors.New("privacy directory permissions could not be secured"))
		}
	}
	return nil
}

func rejectUnsafeExistingTarget(path, op string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return filesystemFault(op, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fault.New(fault.PermissionDenied, op, errors.New("privacy file must not be a symlink"))
	}
	if !info.Mode().IsRegular() {
		return fault.New(fault.InvalidInput, op, errors.New("privacy path is not a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fault.New(fault.PermissionDenied, op, errors.New("privacy file is accessible to group or world"))
	}
	return nil
}

func encodeDocument(snapshot privacy.Snapshot) ([]byte, error) {
	doc := document{
		SchemaVersion: schemaVersion,
		Revision:      snapshot.Revision,
		Grants:        make([]grantDocument, len(snapshot.Grants)),
	}
	for index, grant := range snapshot.Grants {
		doc.Grants[index] = grantDocument{
			Permission: grant.Permission,
			Enabled:    grant.Enabled,
			UpdatedAt:  grant.UpdatedAt,
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode versioned privacy document: %w", err)
	}
	return append(encoded, '\n'), nil
}

func decodeDocument(encoded []byte) (privacy.Snapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var doc document
	if err := decoder.Decode(&doc); err != nil {
		return privacy.Snapshot{}, fmt.Errorf("decode versioned privacy document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON documents are not allowed")
		}
		return privacy.Snapshot{}, fmt.Errorf("reject trailing privacy document: %w", err)
	}
	if doc.SchemaVersion != schemaVersion {
		return privacy.Snapshot{}, fmt.Errorf("unsupported privacy schema %q", doc.SchemaVersion)
	}
	snapshot := privacy.Snapshot{
		Revision: doc.Revision,
		Grants:   make([]privacy.Grant, len(doc.Grants)),
	}
	for index, grant := range doc.Grants {
		snapshot.Grants[index] = privacy.Grant{
			Permission: grant.Permission,
			Enabled:    grant.Enabled,
			UpdatedAt:  grant.UpdatedAt,
		}
	}
	return snapshot, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateContext(op string, ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return contextFault(op, err)
	}
	return nil
}

func contextFault(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func filesystemFault(op string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fault.New(fault.PermissionDenied, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}
