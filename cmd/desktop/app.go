package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"proactive-interaction-engine/adapters/embodiment/webavatar"
	"proactive-interaction-engine/adapters/model/local"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/adapters/storage/privacyfile"
	"proactive-interaction-engine/adapters/tts/speechdispatcher"
	desktopui "proactive-interaction-engine/adapters/ui/desktop"
	"proactive-interaction-engine/adapters/ui/desktopconnect"
	"proactive-interaction-engine/adapters/ui/webpanel"
	"proactive-interaction-engine/gen/go/proactive/platform/v1/platformv1connect"
	application "proactive-interaction-engine/internal/application/engine"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
	"proactive-interaction-engine/internal/runtime/lifecycle"

	"connectrpc.com/connect"
)

const (
	desktopProtocolVersion = "v1"
	maxAssetBytes          = 64 << 20
)

// App owns the loopback listener, HTTP transport, and runtime Runner.
type App struct {
	listener        net.Listener
	server          *http.Server
	runner          *lifecycle.Runner
	origin          string
	token           string
	shutdownTimeout time.Duration
	runMu           sync.Mutex
	started         bool
}

// Build validates and constructs the complete base desktop experience before
// starting any long-running goroutine.
func Build(config Config) (_ *App, err error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	wasm, err := loadAsset(config.WASMFile)
	if err != nil {
		return nil, fmt.Errorf("load desktop WASM: %w", err)
	}
	wasmExec, err := loadAsset(config.WASMExecFile)
	if err != nil {
		return nil, fmt.Errorf("load desktop WASM runtime: %w", err)
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("bind desktop loopback listener: %w", err)
	}
	keepListener := false
	defer func() {
		if !keepListener {
			_ = listener.Close()
		}
	}()

	origin := "http://" + listener.Addr().String()
	token, err := desktopui.GenerateToken(rand.Reader)
	if err != nil {
		return nil, err
	}
	clock := engineclock.System{}
	repository, err := privacyfile.New(config.PrivacyFile)
	if err != nil {
		return nil, err
	}
	permissions, err := privacy.New(context.Background(), repository, clock)
	if err != nil {
		return nil, err
	}

	var speaker webavatar.Speaker
	if config.TTSBinary != "" {
		if err := validateExecutable(config.TTSBinary); err != nil {
			return nil, err
		}
		speaker, err = speechdispatcher.New(config.TTSBinary, speechdispatcher.ExecRunner{})
		if err != nil {
			return nil, err
		}
	}
	driver, err := webavatar.New(local.NewTemplates(), speaker, clock)
	if err != nil {
		return nil, err
	}
	core, err := application.New(application.Config{
		SubjectID:              config.SubjectID,
		ReturnAbsenceThreshold: config.ReturnAbsenceThreshold,
		RejectionCooldown:      config.RejectionCooldown,
		NoResponseCooldown:     config.NoResponseCooldown,
		ActionTimeout:          config.ActionTimeout,
		ExternalCallTimeout:    config.ExternalCallTimeout,
		PolicyVersion:          "policy.v1",
		BehaviorVersion:        "welcome_after_return.v1",
		ConfigHash:             "desktop.runtime.v1",
		RandomSeed:             1,
	}, driver, &memorystorage.AuditRecorder{}, clock)
	if err != nil {
		return nil, err
	}
	runner, err := lifecycle.New(lifecycle.DefaultConfig(), core, clock)
	if err != nil {
		return nil, err
	}
	authorizer, err := desktopui.NewLoopbackAuthorizer(token, origin)
	if err != nil {
		return nil, err
	}
	desktopService, err := desktopui.NewServer(config.SubjectID, driver, permissions, runner, authorizer, clock)
	if err != nil {
		return nil, err
	}
	bridge, err := desktopconnect.New(desktopService)
	if err != nil {
		return nil, err
	}
	panel, err := webpanel.New(token, wasm, wasmExec)
	if err != nil {
		return nil, err
	}

	connectPath, connectHandler := platformv1connect.NewDesktopControlServiceHandler(
		bridge,
		connect.WithRequireConnectProtocolHeader(),
	)
	mux := http.NewServeMux()
	mux.Handle(connectPath, connectHandler)
	mux.Handle("/", panel)
	keepListener = true
	return &App{
		listener: listener,
		server: &http.Server{
			Handler:           desktopconnect.PeerContext(mux),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
		runner:          runner,
		origin:          origin,
		token:           token,
		shutdownTimeout: config.ShutdownTimeout,
	}, nil
}

// URL returns the loopback panel origin without exposing the bearer token.
func (a *App) URL() string { return a.origin }

// Run owns and joins the HTTP server and Runner. UI requests are stopped and
// joined before Runner shutdown asks the output driver to StopAll.
func (a *App) Run(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, "run desktop app", errors.New("context is required"))
	}
	a.runMu.Lock()
	if a.started {
		a.runMu.Unlock()
		return fault.New(fault.InvalidInput, "run desktop app", errors.New("desktop app may only run once"))
	}
	a.started = true
	a.runMu.Unlock()
	return a.run(ctx)
}

func (a *App) run(ctx context.Context) error {
	uiCtx, cancelUI := context.WithCancel(context.Background())
	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	a.server.BaseContext = func(net.Listener) context.Context { return uiCtx }

	runnerDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { runnerDone <- a.runner.Run(runnerCtx) }()
	go func() { serverDone <- a.server.Serve(a.listener) }()

	var firstErr error
	serverFinished := false
	runnerFinished := false
	select {
	case <-ctx.Done():
		firstErr = ctx.Err()
	case err := <-serverDone:
		serverFinished = true
		if !errors.Is(err, http.ErrServerClosed) {
			firstErr = err
		}
	case err := <-runnerDone:
		runnerFinished = true
		if !errors.Is(err, context.Canceled) {
			firstErr = err
		}
	}

	cancelUI()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), a.shutdownTimeout)
	shutdownErr := a.server.Shutdown(shutdownCtx)
	cancelShutdown()
	if shutdownErr != nil {
		_ = a.server.Close()
		if firstErr == nil {
			firstErr = shutdownErr
		}
	}
	if !serverFinished {
		if err := <-serverDone; !errors.Is(err, http.ErrServerClosed) && firstErr == nil {
			firstErr = err
		}
	}

	cancelRunner()
	if !runnerFinished {
		if err := <-runnerDone; !errors.Is(err, context.Canceled) && firstErr == nil {
			firstErr = err
		}
	}
	if errors.Is(firstErr, context.Canceled) {
		return nil
	}
	return firstErr
}

func loadAsset(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("asset must be a regular non-symlink file")
	}
	if info.Size() <= 0 || info.Size() > maxAssetBytes {
		return nil, errors.New("asset size is invalid")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("asset changed during secure open")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || len(content) > maxAssetBytes {
		return nil, errors.New("asset size is invalid")
	}
	return content, nil
}

func validateExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect TTS binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fault.New(fault.InvalidInput, "validate TTS binary", errors.New("TTS binary must be an executable regular non-symlink file"))
	}
	return nil
}
