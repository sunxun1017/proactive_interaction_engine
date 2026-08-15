package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/domain/fault"
)

const (
	maxDesktopIdentityStringBytes = 256
	maxDesktopIdentityCapacity    = 1024
)

// IdentityConfig contains the complete opt-in desktop identity composition.
// Disabled identity has no dormant configuration and enabled identity has no
// implicit profile, storage, policy, lifetime, or capacity defaults.
type IdentityConfig struct {
	Enabled                       bool
	BiometricProfileRef           string
	VaultRoot                     string
	SecretServiceRuntimeDirectory string
	Policy                        identity.Policy
	Identification                identity.IdentificationCoordinatorConfig
	Verification                  identity.SpeakerVerificationCoordinatorConfig
	MaxTrackedSources             int
}

// Config contains only explicit local composition values.
type Config struct {
	SubjectID              string
	ListenAddress          string
	PrivacyFile            string
	ScenarioFile           string
	WASMFile               string
	WASMExecFile           string
	TTSBinary              string
	RuntimeBaseDir         string
	MediaPython            string
	MediaRoot              string
	CameraDevice           string
	ParecBinary            string
	ReturnAbsenceThreshold time.Duration
	RejectionCooldown      time.Duration
	NoResponseCooldown     time.Duration
	ActionTimeout          time.Duration
	ExternalCallTimeout    time.Duration
	ShutdownTimeout        time.Duration
	ProviderLeaseDuration  time.Duration
	ProviderHealthInterval time.Duration
	WorkerStopTimeout      time.Duration
	Identity               IdentityConfig
}

func (c Config) Validate() error {
	const op = "validate desktop configuration"
	if strings.TrimSpace(c.SubjectID) == "" {
		return fault.New(fault.InvalidInput, op, errors.New("subject id is required"))
	}
	host, port, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return fault.New(fault.InvalidInput, op, errors.New("listen address must include an IP and port"))
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fault.New(fault.InvalidInput, op, errors.New("listen address must use an explicit loopback IP"))
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 0 || parsedPort > 65535 {
		return fault.New(fault.InvalidInput, op, errors.New("listen port is invalid"))
	}
	for _, item := range []struct {
		label string
		path  string
	}{
		{label: "privacy file", path: c.PrivacyFile},
		{label: "scenario file", path: c.ScenarioFile},
		{label: "WASM file", path: c.WASMFile},
		{label: "wasm_exec file", path: c.WASMExecFile},
		{label: "runtime base directory", path: c.RuntimeBaseDir},
		{label: "media Python", path: c.MediaPython},
		{label: "media root", path: c.MediaRoot},
		{label: "camera device", path: c.CameraDevice},
		{label: "parec binary", path: c.ParecBinary},
	} {
		if strings.TrimSpace(item.path) == "" || !filepath.IsAbs(item.path) {
			return fault.New(fault.InvalidInput, op, fmt.Errorf("%s must be an absolute path", item.label))
		}
	}
	if c.TTSBinary != "" && !filepath.IsAbs(c.TTSBinary) {
		return fault.New(fault.InvalidInput, op, errors.New("TTS binary must be an absolute path"))
	}
	if c.ReturnAbsenceThreshold <= 0 || c.RejectionCooldown <= 0 || c.NoResponseCooldown <= 0 ||
		c.ActionTimeout <= 0 || c.ExternalCallTimeout <= 0 || c.ShutdownTimeout <= 0 ||
		c.ProviderLeaseDuration <= 0 || c.ProviderHealthInterval <= 0 || c.WorkerStopTimeout <= 0 {
		return fault.New(fault.InvalidInput, op, errors.New("desktop durations must be positive"))
	}
	if err := c.Identity.validate(); err != nil {
		return fault.New(fault.InvalidInput, op, err)
	}
	return nil
}

func (c IdentityConfig) validate() error {
	if !c.Enabled {
		if c != (IdentityConfig{}) {
			return errors.New("disabled identity configuration must not contain dormant values")
		}
		return nil
	}
	if !validDesktopIdentityString(c.BiometricProfileRef) {
		return fmt.Errorf("biometric profile reference must be non-empty, exact, and at most %d bytes", maxDesktopIdentityStringBytes)
	}
	if !validDesktopIdentityString(c.Policy.Version) {
		return fmt.Errorf("identity policy version must be non-empty, exact, and at most %d bytes", maxDesktopIdentityStringBytes)
	}
	if err := validateDesktopIdentityPath("biometric vault root", c.VaultRoot); err != nil {
		return err
	}
	if err := validateDesktopIdentityPath("Secret Service runtime directory", c.SecretServiceRuntimeDirectory); err != nil {
		return err
	}
	if identityPathsOverlap(c.VaultRoot, c.SecretServiceRuntimeDirectory) {
		return errors.New("biometric vault and Secret Service runtime directories must be separate")
	}
	if err := identity.ValidateAt(c.Policy, identity.Evidence{}, time.Unix(1, 0).UTC()); err != nil {
		return fmt.Errorf("validate identity policy: %w", err)
	}
	if !c.Policy.RequireFaceLiveness {
		return errors.New("production identity requires explicit face liveness")
	}
	if c.Policy.MaxEvidenceSkew > c.Policy.MaxEvidenceAge {
		return errors.New("identity evidence skew must not exceed maximum evidence age")
	}
	if c.Identification.WindowDuration > c.Policy.MaxEvidenceAge {
		return errors.New("identification window duration must not exceed maximum evidence age")
	}
	if c.Verification.ChallengeDuration > c.Policy.MaxEvidenceAge {
		return errors.New("speaker verification challenge duration must not exceed maximum evidence age")
	}
	if _, err := identity.NewEvidenceCoordinator(c.Identification); err != nil {
		return fmt.Errorf("validate identification coordinator: %w", err)
	}
	if _, err := identity.NewSpeakerVerificationCoordinator(c.Verification); err != nil {
		return fmt.Errorf("validate speaker verification coordinator: %w", err)
	}
	if c.MaxTrackedSources <= 0 || c.MaxTrackedSources > maxDesktopIdentityCapacity {
		return fmt.Errorf("maximum tracked identity sources must be within [1,%d]", maxDesktopIdentityCapacity)
	}
	return nil
}

func validDesktopIdentityString(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maxDesktopIdentityStringBytes
}

func validateDesktopIdentityPath(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an explicit absolute path", label)
	}
	cleaned := filepath.Clean(value)
	if cleaned != value {
		return fmt.Errorf("%s must be canonical", label)
	}
	if cleaned == filepath.VolumeName(cleaned)+string(os.PathSeparator) {
		return fmt.Errorf("%s cannot be a filesystem root", label)
	}
	return nil
}

func identityPathsOverlap(left, right string) bool {
	return identityPathContains(left, right) || identityPathContains(right, left)
}

func identityPathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return true
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}
