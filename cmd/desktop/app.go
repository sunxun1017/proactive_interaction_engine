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
	"path/filepath"
	"sync"
	"syscall"
	"time"

	capabilityregistry "proactive-interaction-engine/adapters/capability/registry"
	scenarioconfig "proactive-interaction-engine/adapters/config/scenario"
	"proactive-interaction-engine/adapters/embodiment/webavatar"
	inputingress "proactive-interaction-engine/adapters/input/ingress"
	"proactive-interaction-engine/adapters/model/local"
	"proactive-interaction-engine/adapters/storage/biometricvault"
	memorystorage "proactive-interaction-engine/adapters/storage/memory"
	"proactive-interaction-engine/adapters/storage/privacyfile"
	"proactive-interaction-engine/adapters/transport/localgrpc"
	"proactive-interaction-engine/adapters/tts/speechdispatcher"
	desktopui "proactive-interaction-engine/adapters/ui/desktop"
	"proactive-interaction-engine/adapters/ui/desktopconnect"
	"proactive-interaction-engine/adapters/ui/webpanel"
	"proactive-interaction-engine/adapters/worker/supervisor"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
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
	workers         *supervisor.Supervisor
	workerTransport *localgrpc.Server
	identity        *desktopIdentityComposition
	origin          string
	token           string
	shutdownTimeout time.Duration
	runMu           sync.Mutex
	started         bool
}

// Build validates and constructs the complete base desktop experience before
// starting any long-running goroutine.
func Build(config Config) (_ *App, err error) {
	return build(config, func(input biometricvault.SecretServiceMasterKeyConfig) (biometricvault.MasterKeyProvider, error) {
		return biometricvault.NewSecretServiceMasterKeyProvider(input)
	})
}

func build(config Config, newMasterKeyProvider identityMasterKeyProviderFactory) (_ *App, err error) {
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
	scenarioFile, err := secureOpen(config.ScenarioFile)
	if err != nil {
		return nil, fmt.Errorf("open scenario manifest: %w", err)
	}
	loadedScenario, loadErr := scenarioconfig.Load(scenarioFile)
	closeScenarioErr := scenarioFile.Close()
	if loadErr != nil {
		return nil, loadErr
	}
	if closeScenarioErr != nil {
		return nil, closeScenarioErr
	}
	for _, path := range []string{config.MediaPython, config.ParecBinary} {
		if err := validateExecutableLink(path); err != nil {
			return nil, err
		}
	}
	if info, err := os.Lstat(config.MediaRoot); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fault.New(fault.InvalidInput, "validate media root", errors.New("media root must be a real directory"))
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
	registry, err := capabilityregistry.New(clock, config.ProviderLeaseDuration)
	if err != nil {
		return nil, err
	}
	var identityComposition *desktopIdentityComposition
	if config.Identity.Enabled {
		identityContext, cancelIdentity := context.WithTimeout(context.Background(), config.ExternalCallTimeout)
		identityComposition, err = newDesktopIdentityComposition(
			identityContext, config.Identity, permissions, registry, clock, loadedScenario.Requirements, newMasterKeyProvider,
		)
		cancelIdentity()
		if err != nil {
			return nil, err
		}
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
		ConfigHash:             loadedScenario.Hash,
		RandomSeed:             1,
	}, driver, &memorystorage.AuditRecorder{}, clock)
	if err != nil {
		return nil, err
	}
	runner, err := lifecycle.New(lifecycle.DefaultConfig(), core, clock)
	if err != nil {
		return nil, err
	}
	activationOptions := activationViewOptions{TTSEnabled: speaker != nil}
	if identityComposition != nil {
		activationOptions.Identity = identityComposition.readiness
	}
	activation, err := newActivationView(loadedScenario.Requirements, registry, clock, activationOptions)
	if err != nil {
		return nil, err
	}
	ingress, err := inputingress.NewServer(registry, runner, core, activation, clock, loadedScenario.Requirements)
	if err != nil {
		return nil, err
	}
	workerTransport, err := localgrpc.New(config.RuntimeBaseDir)
	if err != nil {
		return nil, err
	}
	keepWorkerTransport := false
	defer func() {
		if !keepWorkerTransport {
			_ = workerTransport.Close()
		}
	}()
	platformv1.RegisterCapabilityProviderRegistryServiceServer(workerTransport.GRPC(), registry)
	platformv1.RegisterObservationIngressServiceServer(workerTransport.GRPC(), ingress)
	if identityComposition != nil {
		platformv1.RegisterIdentityEvidenceIngressServiceServer(workerTransport.GRPC(), identityComposition.ingress)
	}
	workers, err := supervisor.New(supervisor.Config{
		StopTimeout:         config.WorkerStopTimeout,
		HealthCheckInterval: config.ProviderHealthInterval,
	}, permissions, registry, supervisor.ExecLauncher{}, clock, mediaWorkerSpecs(config, workerTransport.Address()))
	if err != nil {
		return nil, err
	}
	authorizer, err := desktopui.NewLoopbackAuthorizer(token, origin)
	if err != nil {
		return nil, err
	}
	desktopService, err := desktopui.NewServer(config.SubjectID, driver, workers, workers, runner, authorizer, clock)
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
	keepWorkerTransport = true
	return &App{
		listener: listener,
		server: &http.Server{
			Handler:           desktopconnect.PeerContext(mux),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
		runner:          runner,
		workers:         workers,
		workerTransport: workerTransport,
		identity:        identityComposition,
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
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	transportCtx, cancelTransport := context.WithCancel(context.Background())
	a.server.BaseContext = func(net.Listener) context.Context { return uiCtx }

	runnerDone := make(chan error, 1)
	workerDone := make(chan error, 1)
	transportDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { runnerDone <- a.runner.Run(runnerCtx) }()
	go func() { transportDone <- a.workerTransport.Run(transportCtx) }()
	go func() { workerDone <- a.workers.Run(workerCtx) }()
	go func() { serverDone <- a.server.Serve(a.listener) }()

	var firstErr error
	serverFinished := false
	runnerFinished := false
	workerFinished := false
	transportFinished := false
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
	case err := <-workerDone:
		workerFinished = true
		if err != nil {
			firstErr = err
		}
	case err := <-transportDone:
		transportFinished = true
		if err != nil {
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

	cancelWorkers()
	if !workerFinished {
		if err := <-workerDone; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	cancelTransport()
	if !transportFinished {
		if err := <-transportDone; err != nil && firstErr == nil {
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

func mediaWorkerSpecs(config Config, address string) []supervisor.Spec {
	pythonPath := filepath.Dir(config.MediaPython)
	pythonEnv := "PYTHONPATH=" + filepath.Join(config.MediaRoot, "gen", "python") + string(os.PathListSeparator) + config.MediaRoot
	return []supervisor.Spec{
		{
			ProviderID: "desktop-presence", Permission: privacy.CameraCapture, Command: config.MediaPython,
			WorkingDir: config.MediaRoot, Env: []string{pythonEnv, "PATH=" + pythonPath + string(os.PathListSeparator) + os.Getenv("PATH")},
			Args: []string{"-m", "workers.camera.presence", "--grpc-address", address, "--device", config.CameraDevice, "--subject-id", config.SubjectID},
		},
		{
			ProviderID: "desktop-vad", Permission: privacy.MicrophoneCapture, Command: config.MediaPython,
			WorkingDir: config.MediaRoot, Env: []string{pythonEnv, "PATH=" + pythonPath + string(os.PathListSeparator) + os.Getenv("PATH")},
			Args: []string{"-m", "workers.microphone.activity", "--grpc-address", address, "--parec", config.ParecBinary, "--subject-id", config.SubjectID},
		},
	}
}

func loadAsset(path string) ([]byte, error) {
	file, err := secureOpen(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxAssetBytes {
		return nil, errors.New("asset size is invalid")
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

func secureOpen(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("file must be a regular non-symlink file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, errors.New("file changed during secure open")
	}
	return file, nil
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

func validateExecutableLink(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return fmt.Errorf("inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fault.New(fault.InvalidInput, "validate executable", errors.New("resolved path must be an executable regular file"))
	}
	return nil
}
