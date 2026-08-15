package biometricvault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"proactive-interaction-engine/internal/domain/fault"
)

const maxEncryptedBytes = maxTemplateBytes + (4 << 10)

const vaultLockFilename = ".vault.lock"

type fileIdentity struct {
	device uint64
	inode  uint64
}

var rootMutexes sync.Map

func sharedRootMutex(root string) *sync.Mutex {
	value, _ := rootMutexes.LoadOrStore(root, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func readSecureFile(path, op string) ([]byte, fileIdentity, bool, error) {
	if err := rejectUnsafeDirectoryIfPresent(filepath.Dir(path), op); err != nil {
		return nil, fileIdentity{}, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fileIdentity{}, false, nil
	}
	if err != nil {
		return nil, fileIdentity{}, false, filesystemFault(op, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fileIdentity{}, false, fault.New(fault.PermissionDenied, op, errors.New("protected file must not be a symlink"))
	}
	if !info.Mode().IsRegular() {
		return nil, fileIdentity{}, false, fault.New(fault.InvalidInput, op, errors.New("protected path is not a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fileIdentity{}, false, fault.New(fault.PermissionDenied, op, errors.New("protected file is accessible to group or world"))
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fileIdentity{}, false, fault.New(fault.PermissionDenied, op, errors.New("protected file must not be a symlink"))
		}
		return nil, fileIdentity{}, false, filesystemFault(op, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, fileIdentity{}, false, filesystemFault(op, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fileIdentity{}, false, fault.New(fault.PermissionDenied, op, errors.New("protected file changed during secure open"))
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, fileIdentity{}, false, fault.New(fault.PermissionDenied, op, errors.New("protected file must have exactly one hard link"))
	}
	identity := fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	encoded, err := io.ReadAll(io.LimitReader(file, maxEncryptedBytes+1))
	if err != nil {
		return nil, fileIdentity{}, false, filesystemFault(op, err)
	}
	if len(encoded) > maxEncryptedBytes {
		return nil, fileIdentity{}, false, fault.New(fault.InvalidInput, op, errors.New("encrypted protected file exceeds size limit"))
	}
	return encoded, identity, true, nil
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
			created := false
			if err := os.Mkdir(current, privateDirMode); err == nil {
				created = true
			} else if !errors.Is(err, os.ErrExist) {
				return filesystemFault(op, err)
			}
			if created {
				if err := syncDirectory(filepath.Dir(current)); err != nil {
					return filesystemFault(op, err)
				}
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
		return fault.New(fault.PermissionDenied, op, errors.New("existing vault directory is accessible to group or world"))
	}
	return nil
}

func acquireVaultLock(directory, op string) (func(), error) {
	return acquirePrivateFileLock(directory, vaultLockFilename, "biometric vault", op)
}

func acquirePrivateFileLock(directory, filename, resource, op string) (func(), error) {
	path := filepath.Join(directory, filename)
	before, err := os.Lstat(path)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return nil, filesystemFault(op, err)
	}
	if !created && (before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0) {
		return nil, fault.New(fault.PermissionDenied, op, fmt.Errorf("%s lock file is unsafe", resource))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, privateFileMode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fault.New(fault.PermissionDenied, op, fmt.Errorf("%s lock file must not be a symlink", resource))
		}
		return nil, filesystemFault(op, err)
	}
	closeWithError := func(err error) (func(), error) {
		_ = file.Close()
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return closeWithError(filesystemFault(op, err))
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 || !ok || stat.Nlink != 1 {
		return closeWithError(fault.New(fault.PermissionDenied, op, fmt.Errorf("%s lock file is unsafe", resource)))
	}
	if !created && !os.SameFile(before, opened) {
		return closeWithError(fault.New(fault.PermissionDenied, op, fmt.Errorf("%s lock file changed during secure open", resource)))
	}
	if created {
		if err := file.Sync(); err != nil {
			return closeWithError(filesystemFault(op, err))
		}
		if err := syncDirectory(directory); err != nil {
			return closeWithError(filesystemFault(op, err))
		}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return closeWithError(fault.New(fault.Unavailable, op, fmt.Errorf("%s is busy", resource)))
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func removeAuthenticated(path string, expected fileIdentity, op string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return filesystemFault(op, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ok || stat.Nlink != 1 {
		return fault.New(fault.PermissionDenied, op, errors.New("authenticated file became unsafe before deletion"))
	}
	actual := fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	if actual != expected {
		return fault.New(fault.StaleInput, op, errors.New("authenticated file changed before deletion"))
	}
	if err := os.Remove(path); err != nil {
		return filesystemFault(op, err)
	}
	return nil
}

func rejectUnsafeDirectoryIfPresent(directory, op string) error {
	cleaned := filepath.Clean(directory)
	volume := filepath.VolumeName(cleaned)
	root := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(cleaned, root)
	current := root
	for _, element := range strings.Split(relative, string(os.PathSeparator)) {
		if element == "" || element == "." {
			continue
		}
		current = filepath.Join(current, element)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
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
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return filesystemFault(op, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fault.New(fault.PermissionDenied, op, errors.New("vault directory is accessible to group or world"))
	}
	return nil
}

func createAtomic(ctx context.Context, path string, encoded []byte, op string) error {
	if err := rejectUnsafeExistingTarget(path, op); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".biometric-*.tmp")
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
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fault.New(fault.StaleInput, op, errors.New("template reference was created concurrently"))
		}
		return filesystemFault(op, err)
	}
	if err := syncDirectory(parent); err != nil {
		return filesystemFault(op, err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return filesystemFault(op, err)
	}
	if err := syncDirectory(parent); err != nil {
		return filesystemFault(op, err)
	}
	return nil
}

func cleanupOrphanTemps(directory, prefix, op string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return filesystemFault(op, err)
	}
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return filesystemFault(op, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return fault.New(fault.PermissionDenied, op, errors.New("orphan biometric temporary file is unsafe"))
		}
		if err := os.Remove(path); err != nil {
			return filesystemFault(op, err)
		}
		removed = true
	}
	if removed {
		if err := syncDirectory(directory); err != nil {
			return filesystemFault(op, err)
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
		return fault.New(fault.PermissionDenied, op, errors.New("template file must not be a symlink"))
	}
	if !info.Mode().IsRegular() {
		return fault.New(fault.InvalidInput, op, errors.New("template path is not a regular file"))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fault.New(fault.PermissionDenied, op, errors.New("template file is accessible to group or world"))
	}
	return fault.New(fault.StaleInput, op, errors.New("template reference already exists"))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func filesystemFault(op string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fault.New(fault.PermissionDenied, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}
