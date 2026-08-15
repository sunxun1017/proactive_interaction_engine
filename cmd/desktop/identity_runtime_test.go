package main

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/identity"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestDesktopIdentityRuntimeRecognizesOnlyAtIdentificationDeadline(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(identityPermissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerIdentification))
	catalog := newIdentityCatalogReader(identityCatalogSnapshot(now, "profile-a", readiness.SpeakerIdentification, "speaker-model.v1"))
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, catalog)

	window, err := runtime.OpenIdentificationWindow("identity-window-1")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	if _, err := runtime.identification.SubmitSpeakerIdentificationAt(identity.SpeakerIdentificationFragment{
		WindowToken: window.Token, FragmentID: "speaker-fragment-1", OccurredAt: now,
		Candidates: []identity.SpeakerIdentificationCandidate{{
			ID: "speaker-candidate-1", ProfileRef: "profile-a", Score: 1, ModelVersion: "speaker-model.v1",
		}},
	}, now); err != nil {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v", err)
	}

	early, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup())
	if err != nil || early.Resolved {
		t.Fatalf("AdvanceIdentification(early) = %#v, %v, want no-op", early, err)
	}
	if permissions.Calls() != 0 {
		t.Fatalf("permission reads before deadline = %d, want 0", permissions.Calls())
	}
	if _, ok := runtime.LatestResolution(); ok {
		t.Fatal("LatestResolution() exists before a resolution")
	}

	clock.Advance(time.Second)
	result, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup())
	if err != nil {
		t.Fatalf("AdvanceIdentification(deadline) error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != identity.Recognized || result.Resolution.ProfileRef != "profile-a" || result.Resolution.Reason != identity.ReasonSpeakerIdentified {
		t.Fatalf("AdvanceIdentification(deadline) = %#v", result)
	}
	latest, ok := runtime.LatestResolution()
	if !ok || !reflect.DeepEqual(latest, result.Resolution) {
		t.Fatalf("LatestResolution() = %#v, %t, want %#v", latest, ok, result.Resolution)
	}
}

func TestDesktopIdentityRuntimeVerifiesApplicationOwnedProfile(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(identityPermissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerVerification))
	catalog := newIdentityCatalogReader(identityCatalogSnapshot(now, "profile-verified", readiness.SpeakerVerification, "verification-model.v1"))
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, catalog)

	challenge, err := runtime.IssueSpeakerVerification("challenge-1", "evidence-window-1", "profile-verified")
	if err != nil {
		t.Fatalf("IssueSpeakerVerification() error = %v", err)
	}
	if _, err := runtime.verification.SubmitSpeakerVerificationAt(identity.SpeakerVerificationFragment{
		ChallengeID: challenge.ID, WindowToken: challenge.EvidenceWindowID,
		FragmentID: "verification-fragment-1", OccurredAt: now,
		Candidate: identity.SpeakerVerificationSubmissionCandidate{
			ID: "verification-candidate-1", Score: 1, ModelVersion: "verification-model.v1",
		},
	}, now); err != nil {
		t.Fatalf("SubmitSpeakerVerificationAt() error = %v", err)
	}

	clock.Advance(time.Second)
	result, err := runtime.AdvanceVerification(context.Background(), challenge.Wakeup())
	if err != nil {
		t.Fatalf("AdvanceVerification() error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != identity.Verified || result.Resolution.ProfileRef != "profile-verified" || result.Resolution.Reason != identity.ReasonSpeakerVerified {
		t.Fatalf("AdvanceVerification() = %#v", result)
	}
}

func TestDesktopIdentityRuntimeResolvesEmptyWindowAnonymous(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	runtime := newTestDesktopIdentityRuntime(
		t,
		clock,
		newIdentityPermissionReader(privacy.Snapshot{}),
		newIdentityCatalogReader(biometric.Snapshot{}),
	)
	window, err := runtime.OpenIdentificationWindow("identity-window-empty")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	clock.Advance(time.Second)
	result, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup())
	if err != nil {
		t.Fatalf("AdvanceIdentification() error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != identity.Anonymous || result.Resolution.Reason != identity.ReasonNoMatch {
		t.Fatalf("AdvanceIdentification() = %#v", result)
	}
}

func TestDesktopIdentityRuntimeUsesPermissionStateAtClose(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(identityPermissionSnapshot(now, privacy.MicrophoneCapture, privacy.SpeakerIdentification))
	catalog := newIdentityCatalogReader(identityCatalogSnapshot(now, "profile-a", readiness.SpeakerIdentification, "speaker-model.v1"))
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, catalog)
	window, err := runtime.OpenIdentificationWindow("identity-window-revoked")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	if _, err := runtime.identification.SubmitSpeakerIdentificationAt(identity.SpeakerIdentificationFragment{
		WindowToken: window.Token, FragmentID: "speaker-fragment-revoked", OccurredAt: now,
		Candidates: []identity.SpeakerIdentificationCandidate{{
			ID: "speaker-candidate-revoked", ProfileRef: "profile-a", Score: 1, ModelVersion: "speaker-model.v1",
		}},
	}, now); err != nil {
		t.Fatalf("SubmitSpeakerIdentificationAt() error = %v", err)
	}
	permissions.Set(identityPermissionSnapshot(now))

	clock.Advance(time.Second)
	result, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup())
	if err != nil {
		t.Fatalf("AdvanceIdentification() error = %v", err)
	}
	if !result.Resolved || result.Resolution.Assurance != identity.Anonymous || result.Resolution.Reason != identity.ReasonBiometricPermissionMissing {
		t.Fatalf("AdvanceIdentification() = %#v", result)
	}
}

func TestDesktopIdentityRuntimeKeepsLatestOnNoOpAndReadFailure(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(identityPermissionSnapshot(now))
	catalog := newIdentityCatalogReader(biometric.Snapshot{})
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, catalog)

	first, err := runtime.OpenIdentificationWindow("identity-window-first")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow(first) error = %v", err)
	}
	clock.Advance(time.Second)
	resolved, err := runtime.AdvanceIdentification(context.Background(), first.Wakeup())
	if err != nil || !resolved.Resolved {
		t.Fatalf("AdvanceIdentification(first) = %#v, %v", resolved, err)
	}
	want, ok := runtime.LatestResolution()
	if !ok {
		t.Fatal("LatestResolution() missing first result")
	}

	second, err := runtime.OpenIdentificationWindow("identity-window-second")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow(second) error = %v", err)
	}
	if noOp, err := runtime.AdvanceIdentification(context.Background(), second.Wakeup()); err != nil || noOp.Resolved {
		t.Fatalf("AdvanceIdentification(early second) = %#v, %v", noOp, err)
	}
	if got, _ := runtime.LatestResolution(); !reflect.DeepEqual(got, want) {
		t.Fatalf("latest after no-op = %#v, want %#v", got, want)
	}

	clock.Advance(time.Second)
	permissions.SetError(errors.New("permission unavailable"))
	if _, err := runtime.AdvanceIdentification(context.Background(), second.Wakeup()); err == nil {
		t.Fatal("AdvanceIdentification(permission failure) error = nil")
	}
	if got, _ := runtime.LatestResolution(); !reflect.DeepEqual(got, want) {
		t.Fatalf("latest after read failure = %#v, want %#v", got, want)
	}
}

func TestDesktopIdentityRuntimeLatestResolutionIsRaceSafe(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	runtime := newTestDesktopIdentityRuntime(
		t,
		clock,
		newIdentityPermissionReader(identityPermissionSnapshot(now)),
		newIdentityCatalogReader(biometric.Snapshot{}),
	)
	window, err := runtime.OpenIdentificationWindow("identity-window-concurrent")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	clock.Advance(time.Second)

	const readers = 32
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(readers + 1)
	for range readers {
		go func() {
			defer workers.Done()
			<-start
			for range 32 {
				_, _ = runtime.LatestResolution()
			}
		}()
	}
	go func() {
		defer workers.Done()
		<-start
		_, _ = runtime.AdvanceIdentification(context.Background(), window.Wakeup())
	}()
	close(start)
	workers.Wait()
	if _, ok := runtime.LatestResolution(); !ok {
		t.Fatal("LatestResolution() missing concurrent resolution")
	}
}

func TestNewDesktopIdentityRuntimeRejectsNilAndInvalidDependencies(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(privacy.Snapshot{})
	catalog := newIdentityCatalogReader(biometric.Snapshot{})
	config := validDesktopIdentityRuntimeConfig()

	var typedNilPermissions *identityPermissionReaderFake
	var typedNilCatalog *identityCatalogReaderFake
	tests := []struct {
		name        string
		config      desktopIdentityRuntimeConfig
		clock       *engineclock.Fake
		permissions identityPermissionReader
		catalog     identityCatalogSnapshotReader
	}{
		{name: "nil clock", config: config, permissions: permissions, catalog: catalog},
		{name: "nil permissions", config: config, clock: clock, catalog: catalog},
		{name: "typed nil permissions", config: config, clock: clock, permissions: typedNilPermissions, catalog: catalog},
		{name: "nil catalog", config: config, clock: clock, permissions: permissions},
		{name: "typed nil catalog", config: config, clock: clock, permissions: permissions, catalog: typedNilCatalog},
		{name: "invalid policy", config: func() desktopIdentityRuntimeConfig {
			invalid := config
			invalid.Policy.Version = ""
			return invalid
		}(), clock: clock, permissions: permissions, catalog: catalog},
		{name: "invalid identification config", config: func() desktopIdentityRuntimeConfig {
			invalid := config
			invalid.Identification.WindowDuration = 0
			return invalid
		}(), clock: clock, permissions: permissions, catalog: catalog},
		{name: "invalid verification config", config: func() desktopIdentityRuntimeConfig {
			invalid := config
			invalid.Verification.ChallengeDuration = 0
			return invalid
		}(), clock: clock, permissions: permissions, catalog: catalog},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newDesktopIdentityRuntime(test.config, test.permissions, test.catalog, test.clock)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("newDesktopIdentityRuntime() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestNewDesktopIdentityRuntimeBoundsWindowsByEvidenceAge(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(privacy.Snapshot{})
	catalog := newIdentityCatalogReader(biometric.Snapshot{})

	exact := validDesktopIdentityRuntimeConfig()
	exact.Identification.WindowDuration = exact.Policy.MaxEvidenceAge
	exact.Verification.ChallengeDuration = exact.Policy.MaxEvidenceAge
	if _, err := newDesktopIdentityRuntime(exact, permissions, catalog, clock); err != nil {
		t.Fatalf("newDesktopIdentityRuntime(exact bounds) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*desktopIdentityRuntimeConfig)
	}{
		{
			name: "identification exceeds evidence age",
			mutate: func(config *desktopIdentityRuntimeConfig) {
				config.Identification.WindowDuration = config.Policy.MaxEvidenceAge + time.Nanosecond
			},
		},
		{
			name: "verification exceeds evidence age",
			mutate: func(config *desktopIdentityRuntimeConfig) {
				config.Verification.ChallengeDuration = config.Policy.MaxEvidenceAge + time.Nanosecond
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validDesktopIdentityRuntimeConfig()
			test.mutate(&config)
			_, err := newDesktopIdentityRuntime(config, permissions, catalog, clock)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("newDesktopIdentityRuntime() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestDesktopIdentityRuntimeClassifiesContextAndPermissionFailures(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(privacy.Snapshot{})
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, newIdentityCatalogReader(biometric.Snapshot{}))
	window, err := runtime.OpenIdentificationWindow("identity-window-faults")
	if err != nil {
		t.Fatalf("OpenIdentificationWindow() error = %v", err)
	}
	clock.Advance(time.Second)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.AdvanceIdentification(canceled, window.Wakeup()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("AdvanceIdentification(canceled) error = %v, want Unavailable", err)
	}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	if _, err := runtime.AdvanceIdentification(deadline, window.Wakeup()); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("AdvanceIdentification(deadline) error = %v, want DeadlineExceeded", err)
	}

	permissions.SetError(errors.New("permission unavailable"))
	if _, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("AdvanceIdentification(permission unavailable) error = %v, want Unavailable", err)
	}
	permissions.SetError(context.DeadlineExceeded)
	if _, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup()); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("AdvanceIdentification(permission deadline) error = %v, want DeadlineExceeded", err)
	}
	permissions.SetError(fault.New(fault.PolicyBlocked, "read permissions", errors.New("locked")))
	if _, err := runtime.AdvanceIdentification(context.Background(), window.Wakeup()); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("AdvanceIdentification(typed permission fault) error = %v, want PolicyBlocked preserved", err)
	}
}

func TestDesktopIdentityRuntimeVerificationClassifiesContextAndPermissionFailures(t *testing.T) {
	now := identityRuntimeNow()
	clock := engineclock.NewFake(now)
	permissions := newIdentityPermissionReader(privacy.Snapshot{})
	runtime := newTestDesktopIdentityRuntime(t, clock, permissions, newIdentityCatalogReader(biometric.Snapshot{}))
	challenge, err := runtime.IssueSpeakerVerification("challenge-faults", "evidence-window-faults", "profile-a")
	if err != nil {
		t.Fatalf("IssueSpeakerVerification() error = %v", err)
	}
	clock.Advance(time.Second)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.AdvanceVerification(canceled, challenge.Wakeup()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("AdvanceVerification(canceled) error = %v, want Unavailable", err)
	}
	deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	if _, err := runtime.AdvanceVerification(deadline, challenge.Wakeup()); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("AdvanceVerification(deadline) error = %v, want DeadlineExceeded", err)
	}

	permissions.SetError(errors.New("permission unavailable"))
	if _, err := runtime.AdvanceVerification(context.Background(), challenge.Wakeup()); !fault.IsCode(err, fault.Unavailable) {
		t.Fatalf("AdvanceVerification(permission unavailable) error = %v, want Unavailable", err)
	}
	permissions.SetError(context.DeadlineExceeded)
	if _, err := runtime.AdvanceVerification(context.Background(), challenge.Wakeup()); !fault.IsCode(err, fault.DeadlineExceeded) {
		t.Fatalf("AdvanceVerification(permission deadline) error = %v, want DeadlineExceeded", err)
	}
	permissions.SetError(fault.New(fault.PolicyBlocked, "read permissions", errors.New("locked")))
	if _, err := runtime.AdvanceVerification(context.Background(), challenge.Wakeup()); !fault.IsCode(err, fault.PolicyBlocked) {
		t.Fatalf("AdvanceVerification(typed permission fault) error = %v, want PolicyBlocked preserved", err)
	}
}

func TestDesktopIdentityRuntimeRejectsNilAdvanceContext(t *testing.T) {
	runtime := newTestDesktopIdentityRuntime(
		t,
		engineclock.NewFake(identityRuntimeNow()),
		newIdentityPermissionReader(privacy.Snapshot{}),
		newIdentityCatalogReader(biometric.Snapshot{}),
	)
	if _, err := runtime.AdvanceIdentification(nil, identity.IdentificationWakeup{}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("AdvanceIdentification(nil) error = %v, want InvalidInput", err)
	}
	if _, err := runtime.AdvanceVerification(nil, identity.SpeakerVerificationWakeup{}); !fault.IsCode(err, fault.InvalidInput) {
		t.Fatalf("AdvanceVerification(nil) error = %v, want InvalidInput", err)
	}
}

func newTestDesktopIdentityRuntime(
	t *testing.T,
	clock *engineclock.Fake,
	permissions identityPermissionReader,
	catalog identityCatalogSnapshotReader,
) *desktopIdentityRuntime {
	t.Helper()
	runtime, err := newDesktopIdentityRuntime(validDesktopIdentityRuntimeConfig(), permissions, catalog, clock)
	if err != nil {
		t.Fatalf("newDesktopIdentityRuntime() error = %v", err)
	}
	return runtime
}

func validDesktopIdentityRuntimeConfig() desktopIdentityRuntimeConfig {
	return desktopIdentityRuntimeConfig{
		Policy: identity.Policy{
			Version: "identity-policy.test", FaceIdentificationThreshold: 0.8,
			SpeakerIdentificationThreshold: 0.75, SpeakerVerificationThreshold: 0.9,
			MaxEvidenceAge: 5 * time.Second, MaxEvidenceSkew: time.Second, RequireFaceLiveness: true,
		},
		Identification: identity.IdentificationCoordinatorConfig{WindowDuration: time.Second, MaxOpenWindows: 4},
		Verification:   identity.SpeakerVerificationCoordinatorConfig{ChallengeDuration: time.Second, MaxOpenChallenges: 4},
	}
}

func identityRuntimeNow() time.Time {
	return time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)
}

type identityPermissionReaderFake struct {
	mu       sync.Mutex
	snapshot privacy.Snapshot
	err      error
	calls    int
}

func newIdentityPermissionReader(snapshot privacy.Snapshot) *identityPermissionReaderFake {
	return &identityPermissionReaderFake{snapshot: snapshot}
}

func (r *identityPermissionReaderFake) Current(ctx context.Context) (privacy.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return privacy.Snapshot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return privacy.Snapshot{}, r.err
	}
	copy := r.snapshot
	copy.Grants = append([]privacy.Grant(nil), copy.Grants...)
	return copy, nil
}

func (r *identityPermissionReaderFake) Set(snapshot privacy.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshot = snapshot
	r.err = nil
}

func (r *identityPermissionReaderFake) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *identityPermissionReaderFake) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type identityCatalogReaderFake struct {
	mu       sync.Mutex
	snapshot biometric.Snapshot
}

func newIdentityCatalogReader(snapshot biometric.Snapshot) *identityCatalogReaderFake {
	return &identityCatalogReaderFake{snapshot: snapshot}
}

func (r *identityCatalogReaderFake) Current() biometric.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := r.snapshot
	copy.Records = append([]biometric.Record(nil), copy.Records...)
	return copy
}

func identityPermissionSnapshot(at time.Time, enabled ...privacy.Permission) privacy.Snapshot {
	selected := make(map[privacy.Permission]struct{}, len(enabled))
	for _, permission := range enabled {
		selected[permission] = struct{}{}
	}
	snapshot := privacy.Snapshot{Revision: 1, Grants: make([]privacy.Grant, 0, len(privacy.AllPermissions()))}
	for _, permission := range privacy.AllPermissions() {
		grant := privacy.Grant{Permission: permission}
		if _, ok := selected[permission]; ok {
			grant.Enabled = true
			grant.UpdatedAt = at
		}
		snapshot.Grants = append(snapshot.Grants, grant)
	}
	return snapshot
}

func identityCatalogSnapshot(at time.Time, profile string, capability readiness.CapabilityKind, modelVersion string) biometric.Snapshot {
	return biometric.Snapshot{Revision: 1, Records: []biometric.Record{{
		ProfileRef: profile, Capability: capability, Consented: true, ConsentUpdatedAt: at,
		TemplateRef: "template-ref", ModelVersion: modelVersion,
		Status: biometric.EnrollmentActive, EnrollmentUpdatedAt: at,
	}}}
}
