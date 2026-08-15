package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/gen/go/proactive/platform/v1/platformv1connect"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/domain/fault"
)

func TestConfigValidationFailsBeforeExternalWork(t *testing.T) {
	valid := testConfig(t)
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "empty subject", mutate: func(config *Config) { config.SubjectID = "" }},
		{name: "non loopback listen", mutate: func(config *Config) { config.ListenAddress = "0.0.0.0:0" }},
		{name: "hostname listen", mutate: func(config *Config) { config.ListenAddress = "localhost:0" }},
		{name: "relative privacy", mutate: func(config *Config) { config.PrivacyFile = "privacy.json" }},
		{name: "relative scenario", mutate: func(config *Config) { config.ScenarioFile = "scenario.yaml" }},
		{name: "relative wasm", mutate: func(config *Config) { config.WASMFile = "app.wasm" }},
		{name: "relative wasm exec", mutate: func(config *Config) { config.WASMExecFile = "wasm_exec.js" }},
		{name: "relative tts", mutate: func(config *Config) { config.TTSBinary = "spd-say" }},
		{name: "relative runtime", mutate: func(config *Config) { config.RuntimeBaseDir = "run" }},
		{name: "relative media python", mutate: func(config *Config) { config.MediaPython = "python" }},
		{name: "relative media root", mutate: func(config *Config) { config.MediaRoot = "." }},
		{name: "relative camera", mutate: func(config *Config) { config.CameraDevice = "video0" }},
		{name: "relative parec", mutate: func(config *Config) { config.ParecBinary = "parec" }},
		{name: "zero return threshold", mutate: func(config *Config) { config.ReturnAbsenceThreshold = 0 }},
		{name: "zero rejection cooldown", mutate: func(config *Config) { config.RejectionCooldown = 0 }},
		{name: "zero no response cooldown", mutate: func(config *Config) { config.NoResponseCooldown = 0 }},
		{name: "zero action timeout", mutate: func(config *Config) { config.ActionTimeout = 0 }},
		{name: "zero external timeout", mutate: func(config *Config) { config.ExternalCallTimeout = 0 }},
		{name: "zero shutdown timeout", mutate: func(config *Config) { config.ShutdownTimeout = 0 }},
		{name: "zero provider lease", mutate: func(config *Config) { config.ProviderLeaseDuration = 0 }},
		{name: "zero health interval", mutate: func(config *Config) { config.ProviderHealthInterval = 0 }},
		{name: "zero worker stop timeout", mutate: func(config *Config) { config.WorkerStopTimeout = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if err := config.Validate(); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Validate() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestIdentityConfigValidationIsStrictAndConditional(t *testing.T) {
	valid := testConfig(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(disabled identity) error = %v", err)
	}

	dormant := valid
	dormant.Identity.BiometricProfileRef = "profile-a"
	if err := dormant.Validate(); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("Validate(disabled identity with dormant fields) error = %v, want InvalidInput", err)
	}

	valid.Identity = validIdentityConfig(t)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(enabled identity) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*IdentityConfig)
	}{
		{name: "blank profile", mutate: func(config *IdentityConfig) { config.BiometricProfileRef = "" }},
		{name: "spaced profile", mutate: func(config *IdentityConfig) { config.BiometricProfileRef = " profile-a " }},
		{name: "oversized profile", mutate: func(config *IdentityConfig) { config.BiometricProfileRef = strings.Repeat("a", 257) }},
		{name: "relative vault", mutate: func(config *IdentityConfig) { config.VaultRoot = "vault" }},
		{name: "filesystem root vault", mutate: func(config *IdentityConfig) { config.VaultRoot = "/" }},
		{name: "unclean vault", mutate: func(config *IdentityConfig) { config.VaultRoot += "/../vault" }},
		{name: "relative secret runtime", mutate: func(config *IdentityConfig) { config.SecretServiceRuntimeDirectory = "runtime" }},
		{name: "same storage paths", mutate: func(config *IdentityConfig) { config.SecretServiceRuntimeDirectory = config.VaultRoot }},
		{name: "nested storage paths", mutate: func(config *IdentityConfig) {
			config.SecretServiceRuntimeDirectory = filepath.Join(config.VaultRoot, "runtime")
		}},
		{name: "blank policy version", mutate: func(config *IdentityConfig) { config.Policy.Version = "" }},
		{name: "oversized policy version", mutate: func(config *IdentityConfig) { config.Policy.Version = strings.Repeat("v", 257) }},
		{name: "zero face threshold", mutate: func(config *IdentityConfig) { config.Policy.FaceIdentificationThreshold = 0 }},
		{name: "nan speaker threshold", mutate: func(config *IdentityConfig) { config.Policy.SpeakerIdentificationThreshold = math.NaN() }},
		{name: "face liveness not required", mutate: func(config *IdentityConfig) { config.Policy.RequireFaceLiveness = false }},
		{name: "skew exceeds age", mutate: func(config *IdentityConfig) {
			config.Policy.MaxEvidenceSkew = config.Policy.MaxEvidenceAge + time.Nanosecond
		}},
		{name: "window exceeds age", mutate: func(config *IdentityConfig) {
			config.Identification.WindowDuration = config.Policy.MaxEvidenceAge + time.Nanosecond
		}},
		{name: "challenge exceeds age", mutate: func(config *IdentityConfig) {
			config.Verification.ChallengeDuration = config.Policy.MaxEvidenceAge + time.Nanosecond
		}},
		{name: "zero window capacity", mutate: func(config *IdentityConfig) { config.Identification.MaxOpenWindows = 0 }},
		{name: "excess challenge capacity", mutate: func(config *IdentityConfig) { config.Verification.MaxOpenChallenges = 1025 }},
		{name: "zero tracked sources", mutate: func(config *IdentityConfig) { config.MaxTrackedSources = 0 }},
		{name: "excess tracked sources", mutate: func(config *IdentityConfig) { config.MaxTrackedSources = 1025 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config.Identity)
			if err := config.Validate(); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("Validate() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestLoadAssetRejectsUnsafeOrInvalidFiles(t *testing.T) {
	root := t.TempDir()
	validPath := filepath.Join(root, "valid.asset")
	writeAsset(t, validPath, []byte("valid"))
	loaded, err := loadAsset(validPath)
	if err != nil || string(loaded) != "valid" {
		t.Fatalf("loadAsset(valid) = %q, %v", loaded, err)
	}
	loaded[0] = 'X'
	again, err := loadAsset(validPath)
	if err != nil || string(again) != "valid" {
		t.Fatalf("loadAsset(valid again) = %q, %v, want independent read", again, err)
	}

	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	empty := filepath.Join(root, "empty")
	writeAsset(t, empty, nil)
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(validPath, symlink); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	oversized := filepath.Join(root, "oversized")
	writeAsset(t, oversized, []byte{1})
	if err := os.Truncate(oversized, maxAssetBytes+1); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	for _, path := range []string{filepath.Join(root, "missing"), directory, empty, symlink, oversized} {
		if _, err := loadAsset(path); err == nil {
			t.Fatalf("loadAsset(%q) error = nil, want rejection", path)
		}
	}
}

func TestBuildRunsPanelAndTypedGRPCWebThenShutsDown(t *testing.T) {
	app, err := Build(testConfig(t))
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	panelRequest, err := http.NewRequestWithContext(context.Background(), http.MethodGet, app.URL(), nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	panelResponse, err := http.DefaultClient.Do(panelRequest)
	if err != nil {
		t.Fatalf("GET panel error = %v", err)
	}
	defer panelResponse.Body.Close()
	if panelResponse.StatusCode != http.StatusOK || !strings.HasPrefix(panelResponse.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET panel = %d %q, want HTML 200", panelResponse.StatusCode, panelResponse.Header.Get("Content-Type"))
	}

	client := platformv1connect.NewDesktopControlServiceClient(http.DefaultClient, app.URL(), connect.WithGRPCWeb())
	stateRequest := connect.NewRequest(&platformv1.GetStateRequest{ProtocolVersion: desktopProtocolVersion})
	stateRequest.Header().Set("authorization", "Bearer "+app.token)
	stateRequest.Header().Set("origin", app.URL())
	stateResponse, err := client.GetState(context.Background(), stateRequest)
	if err != nil {
		t.Fatalf("GetState() error = %v", err)
	}
	if state := stateResponse.Msg.GetState(); state.GetRevision() == 0 || state.GetAvatar().GetMode() != platformv1.AvatarMode_AVATAR_MODE_IDLE || len(state.GetPermissions()) != 8 {
		t.Fatalf("GetState() = %#v, want initial IDLE with 8 permissions", state)
	} else if len(state.GetProviders()) != 2 ||
		state.GetProviders()[0].GetProviderId() != "desktop-presence" ||
		state.GetProviders()[0].GetState() != platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DISABLED ||
		state.GetProviders()[1].GetProviderId() != "desktop-vad" ||
		state.GetProviders()[1].GetState() != platformv1.ProviderRuntimeState_PROVIDER_RUNTIME_STATE_DISABLED {
		t.Fatalf("GetState() providers = %#v, want two disabled media providers", state.GetProviders())
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not join after cancellation")
	}
}

func TestMediaWorkerSpecsAreExplicitAndUsePrivateUDS(t *testing.T) {
	config := testConfig(t)
	specs := mediaWorkerSpecs(config, "unix:///run/user/1000/private/workers.sock")
	if len(specs) != 2 {
		t.Fatalf("mediaWorkerSpecs() = %#v, want two", specs)
	}
	for _, spec := range specs {
		if spec.Command != config.MediaPython || spec.WorkingDir != config.MediaRoot || len(spec.Env) != 2 {
			t.Fatalf("worker spec = %#v, want explicit Python environment", spec)
		}
		joined := strings.Join(spec.Args, " ")
		if !strings.Contains(joined, "--grpc-address unix:///run/user/1000/private/workers.sock") || strings.Contains(joined, "UserReply") {
			t.Fatalf("worker args = %q, want only private UDS transport", joined)
		}
	}
	if specs[0].ProviderID != "desktop-presence" || specs[1].ProviderID != "desktop-vad" {
		t.Fatalf("provider order = %#v", specs)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	wasm := filepath.Join(root, "app.wasm")
	wasmExec := filepath.Join(root, "wasm_exec.js")
	scenario := filepath.Join(root, "scenario.yaml")
	runtimeDir, err := os.MkdirTemp("/tmp", "pie-")
	if err != nil {
		t.Fatalf("MkdirTemp(runtime) error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	writeAsset(t, wasm, []byte{0, 'a', 's', 'm'})
	writeAsset(t, wasmExec, []byte("globalThis.Go = class Go {};"))
	writeAsset(t, scenario, []byte(`schema_version: v2
scenario:
  id: anonymous-return-welcome
  version: v2
  minimum_identity_assurance: ANONYMOUS
  required:
    - capability: PERSON_PRESENCE
      provider_id: desktop-presence
      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL]
        maximum_latency: 3s
        allowed_cancellation_semantics: [COOPERATIVE]
        allowed_device_classes: [CAMERA]
    - capability: VOICE_ACTIVITY
      provider_id: desktop-vad
      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL]
        maximum_latency: 1s
        allowed_cancellation_semantics: [COOPERATIVE]
        allowed_device_classes: [MICROPHONE]
    - capability: DISPLAY_TEXT
      provider_id: web-avatar
      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL]
        maximum_latency: 100ms
        allowed_cancellation_semantics: [COOPERATIVE]
        allowed_device_classes: [DISPLAY]
  optional:
    - capability: SPEECH_SYNTHESIS
      provider_id: speech-dispatcher
      fallback: VISUAL_ONLY
      compatibility:
        protocol_version: v1
        allowed_privacy_classes: [DEVICE_LOCAL]
        maximum_latency: 2s
        allowed_cancellation_semantics: [COOPERATIVE]
        allowed_device_classes: [AUDIO_OUTPUT]
`))
	return Config{
		SubjectID:              "user-1",
		ListenAddress:          "127.0.0.1:0",
		PrivacyFile:            filepath.Join(root, "private", "permissions.json"),
		ScenarioFile:           scenario,
		WASMFile:               wasm,
		WASMExecFile:           wasmExec,
		RuntimeBaseDir:         runtimeDir,
		MediaPython:            "/usr/bin/python3.10",
		MediaRoot:              root,
		CameraDevice:           "/dev/video0",
		ParecBinary:            "/usr/bin/pacat",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          2 * time.Second,
		ExternalCallTimeout:    time.Second,
		ShutdownTimeout:        time.Second,
		ProviderLeaseDuration:  15 * time.Second,
		ProviderHealthInterval: time.Second,
		WorkerStopTimeout:      time.Second,
	}
}

func validIdentityConfig(t *testing.T) IdentityConfig {
	t.Helper()
	root := t.TempDir()
	return IdentityConfig{
		Enabled:                       true,
		BiometricProfileRef:           "profile-a",
		VaultRoot:                     filepath.Join(root, "vault"),
		SecretServiceRuntimeDirectory: filepath.Join(t.TempDir(), "secret-service-runtime"),
		Policy: identity.Policy{
			Version: "identity-policy.test", FaceIdentificationThreshold: 0.8,
			SpeakerIdentificationThreshold: 0.75, SpeakerVerificationThreshold: 0.9,
			MaxEvidenceAge: 5 * time.Second, MaxEvidenceSkew: time.Second, RequireFaceLiveness: true,
		},
		Identification:    identity.IdentificationCoordinatorConfig{WindowDuration: time.Second, MaxOpenWindows: 4},
		Verification:      identity.SpeakerVerificationCoordinatorConfig{ChallengeDuration: time.Second, MaxOpenChallenges: 4},
		MaxTrackedSources: 16,
	}
}

func writeAsset(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
