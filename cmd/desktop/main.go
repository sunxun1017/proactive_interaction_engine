// Command desktop runs the first local Ubuntu desktop composition.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	configPath, err := os.UserConfigDir()
	if err != nil {
		fail(err)
	}
	defaults := Config{
		SubjectID:              "user-1",
		ListenAddress:          "127.0.0.1:0",
		PrivacyFile:            filepath.Join(configPath, "proactive-interaction-engine", "privacy.v1.json"),
		ScenarioFile:           absoluteOrInput("configs/scenarios/anonymous-return-welcome.v2.yaml"),
		WASMFile:               absoluteOrInput("dist/web/app.wasm"),
		WASMExecFile:           absoluteOrInput("dist/web/wasm_exec.js"),
		TTSBinary:              "/usr/bin/spd-say",
		RuntimeBaseDir:         filepath.Join("/run/user", fmt.Sprint(os.Getuid())),
		MediaPython:            absoluteOrInput(".conda-gpu/bin/python"),
		MediaRoot:              absoluteOrInput("."),
		CameraDevice:           "/dev/video0",
		ParecBinary:            "/usr/bin/pacat",
		ReturnAbsenceThreshold: 30 * time.Minute,
		RejectionCooldown:      30 * time.Minute,
		NoResponseCooldown:     5 * time.Minute,
		ActionTimeout:          5 * time.Second,
		ExternalCallTimeout:    2 * time.Second,
		ShutdownTimeout:        3 * time.Second,
		ProviderLeaseDuration:  15 * time.Second,
		ProviderHealthInterval: time.Second,
		WorkerStopTimeout:      3 * time.Second,
	}
	flag.StringVar(&defaults.SubjectID, "subject", defaults.SubjectID, "local subject identifier")
	flag.StringVar(&defaults.ListenAddress, "listen", defaults.ListenAddress, "explicit loopback IP and port")
	flag.StringVar(&defaults.PrivacyFile, "privacy-file", defaults.PrivacyFile, "absolute privacy permission file")
	flag.StringVar(&defaults.ScenarioFile, "scenario", defaults.ScenarioFile, "absolute validated scenario manifest")
	flag.StringVar(&defaults.WASMFile, "web-wasm", defaults.WASMFile, "absolute panel WASM asset")
	flag.StringVar(&defaults.WASMExecFile, "wasm-exec", defaults.WASMExecFile, "absolute Go WASM runtime asset")
	flag.StringVar(&defaults.TTSBinary, "tts-binary", defaults.TTSBinary, "absolute Speech Dispatcher binary; empty disables speech")
	flag.StringVar(&defaults.RuntimeBaseDir, "runtime-dir", defaults.RuntimeBaseDir, "absolute private user runtime base directory")
	flag.StringVar(&defaults.MediaPython, "media-python", defaults.MediaPython, "absolute isolated media-worker Python")
	flag.StringVar(&defaults.MediaRoot, "media-root", defaults.MediaRoot, "absolute repository media-worker root")
	flag.StringVar(&defaults.CameraDevice, "camera", defaults.CameraDevice, "explicit absolute V4L2 camera device")
	flag.StringVar(&defaults.ParecBinary, "parec", defaults.ParecBinary, "absolute PulseAudio capture binary")
	flag.BoolVar(&defaults.Identity.Enabled, "identity-enabled", false, "enable the explicit local biometric identity composition")
	flag.StringVar(&defaults.Identity.BiometricProfileRef, "identity-profile", "", "explicit biometric profile reference; required when identity is enabled")
	flag.StringVar(&defaults.Identity.VaultRoot, "identity-vault", "", "absolute encrypted biometric vault root; required when identity is enabled")
	flag.StringVar(&defaults.Identity.SecretServiceRuntimeDirectory, "identity-secret-runtime", "", "absolute private Secret Service coordination directory; required when identity is enabled")
	flag.StringVar(&defaults.Identity.Policy.Version, "identity-policy-version", "", "explicit calibrated identity policy version")
	flag.Float64Var(&defaults.Identity.Policy.FaceIdentificationThreshold, "identity-face-threshold", 0, "explicit calibrated face identification threshold in (0,1]")
	flag.Float64Var(&defaults.Identity.Policy.SpeakerIdentificationThreshold, "identity-speaker-threshold", 0, "explicit calibrated speaker identification threshold in (0,1]")
	flag.Float64Var(&defaults.Identity.Policy.SpeakerVerificationThreshold, "identity-verification-threshold", 0, "explicit calibrated speaker verification threshold in (0,1]")
	flag.DurationVar(&defaults.Identity.Policy.MaxEvidenceAge, "identity-max-evidence-age", 0, "maximum identity evidence age; required when identity is enabled")
	flag.DurationVar(&defaults.Identity.Policy.MaxEvidenceSkew, "identity-max-evidence-skew", 0, "maximum cross-modality evidence skew; required when identity is enabled")
	flag.BoolVar(&defaults.Identity.Policy.RequireFaceLiveness, "identity-require-face-liveness", false, "require independent face liveness evidence")
	flag.DurationVar(&defaults.Identity.Identification.WindowDuration, "identity-window-duration", 0, "identification evidence window duration; required when identity is enabled")
	flag.IntVar(&defaults.Identity.Identification.MaxOpenWindows, "identity-max-open-windows", 0, "bounded open identification windows; required when identity is enabled")
	flag.DurationVar(&defaults.Identity.Verification.ChallengeDuration, "identity-verification-duration", 0, "speaker verification challenge duration; required when identity is enabled")
	flag.IntVar(&defaults.Identity.Verification.MaxOpenChallenges, "identity-max-open-challenges", 0, "bounded open speaker verification challenges; required when identity is enabled")
	flag.IntVar(&defaults.Identity.MaxTrackedSources, "identity-max-tracked-sources", 0, "bounded identity ingress source streams; required when identity is enabled")
	flag.Parse()

	app, err := Build(defaults)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("desktop panel ready at %s\n", app.URL())
	if err := app.Run(ctx); err != nil {
		fail(err)
	}
}

func absoluteOrInput(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
