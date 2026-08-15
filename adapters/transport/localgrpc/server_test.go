package localgrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

func TestNewRejectsUnsafeRuntimeBaseDirectory(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "file")
	if err := os.WriteFile(regular, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	wide := filepath.Join(root, "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(private, symlink); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "relative", path: "runtime"},
		{name: "missing", path: filepath.Join(root, "missing")},
		{name: "regular file", path: regular},
		{name: "wide permissions", path: wide},
		{name: "symlink", path: symlink},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(test.path)
			if err == nil || server != nil {
				t.Fatalf("New(%q) = %#v, %v, want error", test.path, server, err)
			}
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("New(%q) error = %v, want InvalidInput", test.path, err)
			}
		})
	}
}

func TestNewCreatesPrivateDirectoryAndUnixSocket(t *testing.T) {
	base := privateBase(t)
	server, err := New(base)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	socketPath := socketPathFromAddress(t, server.Address())
	if filepath.Dir(filepath.Dir(socketPath)) != base || filepath.Dir(socketPath) == base {
		t.Fatalf("socket path %q is not in a private child of %q", socketPath, base)
	}
	assertMode(t, filepath.Dir(socketPath), os.ModeDir|0o700)
	assertMode(t, socketPath, os.ModeSocket|0o600)
	if server.GRPC() == nil {
		t.Fatal("GRPC() = nil")
	}
}

func TestRunServesRegisteredGRPCAndCancellationCleansOwnedPaths(t *testing.T) {
	base := privateBase(t)
	server, err := New(base)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	socketPath := socketPathFromAddress(t, server.Address())
	tempDir := filepath.Dir(socketPath)
	healthv1.RegisterHealthServer(server.GRPC(), health.NewServer())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- server.Run(ctx) }()

	connection, err := grpc.NewClient(
		server.Address(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		waitRun(t, runDone)
		t.Fatalf("NewClient() error = %v", err)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err = healthv1.NewHealthClient(connection).Check(checkCtx, &healthv1.HealthCheckRequest{})
	checkCancel()
	if err != nil {
		_ = connection.Close()
		cancel()
		waitRun(t, runDone)
		t.Fatalf("health Check() error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("connection.Close() error = %v", err)
	}

	cancel()
	waitRun(t, runDone)
	assertMissing(t, socketPath)
	assertMissing(t, tempDir)
	if err := server.Run(context.Background()); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("second Run() error = %v, want InvalidInput", err)
	}
}

func TestCloseBeforeRunIsIdempotentAndCleansOwnedPaths(t *testing.T) {
	server, err := New(privateBase(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	socketPath := socketPathFromAddress(t, server.Address())
	tempDir := filepath.Dir(socketPath)

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	assertMissing(t, socketPath)
	assertMissing(t, tempDir)
	if err := server.Run(context.Background()); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Run() after Close error = %v, want InvalidInput", err)
	}
}

func TestNewCleansTemporaryDirectoryWhenUnixListenFails(t *testing.T) {
	root := privateBase(t)
	longBase := filepath.Join(root, strings.Repeat("a", 80))
	if err := os.Mkdir(longBase, 0o700); err != nil {
		t.Fatal(err)
	}

	server, err := New(longBase)
	if err == nil || server != nil {
		t.Fatalf("New(long socket path) = %#v, %v, want error", server, err)
	}
	entries, readErr := os.ReadDir(longBase)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("New() left temporary entries: %#v", entries)
	}
}

func privateBase(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "localgrpc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test runtime root: %v", err)
		}
	})
	base := filepath.Join(root, "runtime")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	return base
}

func socketPathFromAddress(t *testing.T, address string) string {
	t.Helper()
	const prefix = "unix://"
	if !strings.HasPrefix(address, prefix) {
		t.Fatalf("Address() = %q, want unix:///absolute/path", address)
	}
	path := strings.TrimPrefix(address, prefix)
	if !filepath.IsAbs(path) {
		t.Fatalf("Address() path = %q, want absolute", path)
	}
	return path
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q) error = %v", path, err)
	}
	got := info.Mode() & (os.ModeType | os.ModePerm)
	if got != want {
		t.Fatalf("mode(%q) = %v, want %v", path, got, want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%q) error = %v, want not exist", path, err)
	}
}

func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop")
	}
}
