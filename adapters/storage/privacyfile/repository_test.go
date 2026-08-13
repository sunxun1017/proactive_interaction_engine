package privacyfile

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

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestNewRequiresExplicitAbsolutePath(t *testing.T) {
	for _, path := range []string{"", " ", "privacy.json", filepath.Join("relative", "privacy.json")} {
		t.Run(path, func(t *testing.T) {
			repository, err := New(path)
			if repository != nil || !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("New(%q) = %#v, %v, want nil and InvalidInput", path, repository, err)
			}
		})
	}
}

func TestLoadMissingFileReturnsSafeRevisionZero(t *testing.T) {
	repository := newTestRepository(t, filepath.Join(t.TempDir(), "missing", "privacy.json"))
	snapshot, err := repository.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.Revision != 0 || len(snapshot.Grants) != 0 {
		t.Fatalf("Load() = %#v, want empty revision-zero snapshot", snapshot)
	}
}

func TestSaveAtomicallyCreatesPrivatePathAndRoundTripsV1(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "permissions.json")
	repository := newTestRepository(t, path)
	want := testSnapshot(1, privacy.CameraCapture)
	if err := repository.Save(context.Background(), 0, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(parent) error = %v", err)
	}
	if permissions := parentInfo.Mode().Perm(); permissions != 0o700 {
		t.Fatalf("parent permissions = %#o, want 0700", permissions)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if permissions := fileInfo.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("file permissions = %#o, want 0600", permissions)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("parent entries = %#v, want only final permissions file", entryNames(entries))
	}

	got, err := repository.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"schema_version":"v1"`)) {
		t.Fatalf("persisted document = %s, want schema_version v1", encoded)
	}
}

func TestSaveRejectsRevisionConflictWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.json")
	repository := newTestRepository(t, path)
	original := testSnapshot(1, privacy.CameraCapture)
	if err := repository.Save(context.Background(), 0, original); err != nil {
		t.Fatalf("Save(original) error = %v", err)
	}
	conflicting := testSnapshot(2, privacy.MicrophoneCapture)
	if err := repository.Save(context.Background(), 0, conflicting); !fault.IsCode(err, fault.StaleInput) {
		t.Fatalf("Save(conflict) error = %v, want StaleInput", err)
	}
	got, err := repository.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("Load() = %#v, want original %#v after conflict", got, original)
	}
}

func TestLoadRejectsInvalidOrOversizedDocuments(t *testing.T) {
	valid := `{"schema_version":"v1","revision":0,"grants":[]}`
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "empty", content: nil},
		{name: "malformed", content: []byte(`{"schema_version":`)},
		{name: "unknown root field", content: []byte(`{"schema_version":"v1","revision":0,"grants":[],"extra":true}`)},
		{name: "unknown grant field", content: []byte(`{"schema_version":"v1","revision":1,"grants":[{"permission":"CAMERA_CAPTURE","enabled":true,"updated_at":"2026-08-14T10:00:00Z","extra":true}]}`)},
		{name: "multiple documents", content: []byte(valid + "\n" + valid)},
		{name: "unknown schema", content: []byte(`{"schema_version":"v2","revision":0,"grants":[]}`)},
		{name: "oversized otherwise valid", content: oversizedValidDocument()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "privacy.json")
			writePrivateFile(t, path, test.content)
			repository := newTestRepository(t, path)
			if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Load() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadRejectsSymlinkAndOverlyPermissiveFile(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target.json")
		writePrivateFile(t, target, []byte(`{"schema_version":"v1","revision":0,"grants":[]}`))
		path := filepath.Join(root, "privacy.json")
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}
		repository := newTestRepository(t, path)
		if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Load(symlink) error = %v, want PermissionDenied", err)
		}
	})

	t.Run("group or world readable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "privacy.json")
		writePrivateFile(t, path, []byte(`{"schema_version":"v1","revision":0,"grants":[]}`))
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("Chmod() error = %v", err)
		}
		repository := newTestRepository(t, path)
		if _, err := repository.Load(context.Background()); !fault.IsCode(err, fault.PermissionDenied) {
			t.Fatalf("Load(0644) error = %v, want PermissionDenied", err)
		}
	})
}

func TestCancelledContextHasNoFilesystemSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "privacy.json")
	repository := newTestRepository(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.Load(ctx); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Load(cancelled) error = %v, want Unavailable", err)
	}
	if err := repository.Save(ctx, 0, testSnapshot(1, privacy.CameraCapture)); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("Save(cancelled) error = %v, want Unavailable", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(parent) error = %v, want not exist after cancelled calls", err)
	}
}

func TestConcurrentSaveWithSameExpectedRevisionHasSingleWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.json")
	repository := newTestRepository(t, path)
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var workers sync.WaitGroup
	for _, permission := range []privacy.Permission{privacy.CameraCapture, privacy.MicrophoneCapture} {
		permission := permission
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errorsSeen <- repository.Save(context.Background(), 0, testSnapshot(1, permission))
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)

	var succeeded, conflicted int
	for err := range errorsSeen {
		switch {
		case err == nil:
			succeeded++
		case fault.IsCode(err, fault.StaleInput):
			conflicted++
		default:
			t.Fatalf("Save() error = %v, want nil or StaleInput", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent results = success:%d conflict:%d, want 1 and 1", succeeded, conflicted)
	}
	got, err := repository.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Revision != 1 {
		t.Fatalf("Load().Revision = %d, want 1", got.Revision)
	}
}

func newTestRepository(t *testing.T, path string) privacy.Repository {
	t.Helper()
	repository, err := New(path)
	if err != nil {
		t.Fatalf("New(%q) error = %v", path, err)
	}
	return repository
}

func testSnapshot(revision uint64, enabled privacy.Permission) privacy.Snapshot {
	updatedAt := time.Date(2026, time.August, 14, 10, 0, 0, 0, time.UTC)
	grants := make([]privacy.Grant, 0, len(privacy.AllPermissions()))
	for _, permission := range privacy.AllPermissions() {
		grant := privacy.Grant{Permission: permission}
		if permission == enabled {
			grant.Enabled = true
			grant.UpdatedAt = updatedAt
		}
		grants = append(grants, grant)
	}
	return privacy.Snapshot{Revision: revision, Grants: grants}
}

func oversizedValidDocument() []byte {
	padding := bytes.Repeat([]byte{' '}, 2<<20)
	document := make([]byte, 0, len(padding)+64)
	document = append(document, `{"schema_version":"v1",`...)
	document = append(document, padding...)
	document = append(document, `"revision":0,"grants":[]}`...)
	return document
}

func writePrivateFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}
