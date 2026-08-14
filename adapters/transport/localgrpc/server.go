package localgrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc"
)

const (
	privateDirectoryMode = 0o700
	privateSocketMode    = 0o600
)

type serverState uint8

const (
	stateReady serverState = iota
	stateRunning
	stateClosed
)

// Server owns one private Unix listener, gRPC server, and temporary directory.
type Server struct {
	grpc       *grpc.Server
	listener   *net.UnixListener
	tempDir    string
	socketPath string
	address    string

	mu      sync.Mutex
	state   serverState
	runDone chan struct{}

	cleanupOnce sync.Once
	cleanupErr  error
}

// New creates a private local listener under an explicit secure runtime base.
func New(runtimeBaseDir string, options ...grpc.ServerOption) (*Server, error) {
	const op = "create local worker grpc server"
	base, err := validateRuntimeBase(runtimeBaseDir)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, op, err)
	}
	tempDir, err := os.MkdirTemp(base, "proactive-grpc-")
	if err != nil {
		return nil, fault.New(fault.Unavailable, op, err)
	}
	if err := os.Chmod(tempDir, privateDirectoryMode); err != nil {
		_ = os.Remove(tempDir)
		return nil, fault.New(fault.Unavailable, op, err)
	}

	socketPath := filepath.Join(tempDir, "workers.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		_ = os.Remove(socketPath)
		_ = os.Remove(tempDir)
		return nil, fault.New(fault.Unavailable, op, err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, privateSocketMode); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(tempDir)
		return nil, fault.New(fault.Unavailable, op, err)
	}

	return &Server{
		grpc:       grpc.NewServer(options...),
		listener:   listener,
		tempDir:    tempDir,
		socketPath: socketPath,
		address:    "unix://" + socketPath,
		state:      stateReady,
		runDone:    make(chan struct{}),
	}, nil
}

// Address returns the Python gRPC-compatible Unix target.
func (s *Server) Address() string { return s.address }

// GRPC returns the server on which composition registers worker services
// before calling Run.
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// Run serves once until cancellation, transport failure, or Close.
func (s *Server) Run(ctx context.Context) error {
	const op = "run local worker grpc server"
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	s.mu.Lock()
	if s.state != stateReady {
		s.mu.Unlock()
		return fault.New(fault.InvalidInput, op, errors.New("server may only run once"))
	}
	s.state = stateRunning
	s.mu.Unlock()
	defer close(s.runDone)

	serveDone := make(chan error, 1)
	go func() { serveDone <- s.grpc.Serve(s.listener) }()

	var serveErr error
	select {
	case serveErr = <-serveDone:
	case <-ctx.Done():
		s.grpc.Stop()
		_ = s.listener.Close()
		serveErr = <-serveDone
	}

	s.grpc.Stop()
	_ = s.listener.Close()
	cleanupErr := s.cleanup()
	s.mu.Lock()
	s.state = stateClosed
	s.mu.Unlock()

	if !normalServeError(serveErr) {
		return fault.New(fault.Unavailable, op, serveErr)
	}
	if cleanupErr != nil {
		return fault.New(fault.Unavailable, op, cleanupErr)
	}
	return nil
}

// Close stops and joins a running server, or releases a server that was never
// run. It is idempotent and is safe for composition build-failure cleanup.
func (s *Server) Close() error {
	s.mu.Lock()
	state := s.state
	if state == stateReady {
		s.state = stateClosed
	}
	s.mu.Unlock()

	if state == stateClosed {
		return s.cleanup()
	}
	s.grpc.Stop()
	_ = s.listener.Close()
	if state == stateRunning {
		<-s.runDone
		return s.cleanupErr
	}
	return s.cleanup()
}

func validateRuntimeBase(input string) (string, error) {
	if strings.TrimSpace(input) == "" || input != strings.TrimSpace(input) || !filepath.IsAbs(input) {
		return "", errors.New("runtime base directory must be an explicit absolute path")
	}
	cleaned := filepath.Clean(input)
	info, err := os.Lstat(cleaned)
	if err != nil {
		return "", fmt.Errorf("inspect runtime base directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("runtime base must be a real directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("runtime base must not be accessible to group or world")
	}
	return cleaned, nil
}

func (s *Server) cleanup() error {
	s.cleanupOnce.Do(func() {
		_ = s.listener.Close()
		var failures []error
		if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("remove worker socket: %w", err))
		}
		if err := os.Remove(s.tempDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("remove worker runtime directory: %w", err))
		}
		s.cleanupErr = errors.Join(failures...)
	})
	return s.cleanupErr
}

func normalServeError(err error) bool {
	return err == nil || errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed)
}
