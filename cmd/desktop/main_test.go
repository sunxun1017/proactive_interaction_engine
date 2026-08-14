package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/gen/go/proactive/platform/v1/platformv1connect"
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
		{name: "relative wasm", mutate: func(config *Config) { config.WASMFile = "app.wasm" }},
		{name: "relative wasm exec", mutate: func(config *Config) { config.WASMExecFile = "wasm_exec.js" }},
		{name: "relative tts", mutate: func(config *Config) { config.TTSBinary = "spd-say" }},
		{name: "zero return threshold", mutate: func(config *Config) { config.ReturnAbsenceThreshold = 0 }},
		{name: "zero rejection cooldown", mutate: func(config *Config) { config.RejectionCooldown = 0 }},
		{name: "zero no response cooldown", mutate: func(config *Config) { config.NoResponseCooldown = 0 }},
		{name: "zero action timeout", mutate: func(config *Config) { config.ActionTimeout = 0 }},
		{name: "zero external timeout", mutate: func(config *Config) { config.ExternalCallTimeout = 0 }},
		{name: "zero shutdown timeout", mutate: func(config *Config) { config.ShutdownTimeout = 0 }},
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

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	wasm := filepath.Join(root, "app.wasm")
	wasmExec := filepath.Join(root, "wasm_exec.js")
	writeAsset(t, wasm, []byte{0, 'a', 's', 'm'})
	writeAsset(t, wasmExec, []byte("globalThis.Go = class Go {};"))
	return Config{
		SubjectID:              "user-1",
		ListenAddress:          "127.0.0.1:0",
		PrivacyFile:            filepath.Join(root, "private", "permissions.json"),
		WASMFile:               wasm,
		WASMExecFile:           wasmExec,
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          2 * time.Second,
		ExternalCallTimeout:    time.Second,
		ShutdownTimeout:        time.Second,
	}
}

func writeAsset(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
