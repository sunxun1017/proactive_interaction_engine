package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"
)

func TestActivationViewRequiresHealthyMediaAndSuppliesLocalDisplay(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	clock := engineclock.NewFake(now)
	providers := &snapshotReader{}
	view, err := newActivationView(readiness.ScenarioRequirements{
		ID:                       "desktop",
		MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous,
		Required: []readiness.CapabilityRequirement{
			{Kind: readiness.PersonPresence, ProviderID: "desktop-presence", Compatibility: desktopCompatibility(3*time.Second, readiness.ProviderDeviceCamera)},
			{Kind: readiness.VoiceActivity, ProviderID: "desktop-vad", Compatibility: desktopCompatibility(time.Second, readiness.ProviderDeviceMicrophone)},
			{Kind: readiness.DisplayText, ProviderID: "web-avatar", Compatibility: desktopCompatibility(100*time.Millisecond, readiness.ProviderDeviceDisplay)},
		},
		Optional: []readiness.OptionalCapability{{
			Kind: readiness.SpeechSynthesis, ProviderID: "speech-dispatcher", Fallback: readiness.VisualOnly,
			Compatibility: desktopCompatibility(2*time.Second, readiness.ProviderDeviceAudioOutput),
		}},
	}, providers, clock, activationViewOptions{})
	if err != nil {
		t.Fatalf("newActivationView() error = %v", err)
	}
	if got := view.CurrentActivation(); got.Status != readiness.Blocked {
		t.Fatalf("activation without media = %#v, want BLOCKED", got)
	}

	providers.snapshots = []readiness.ProviderSnapshot{
		validRuntimeProvider("desktop-presence", readiness.PersonPresence, now.Add(time.Minute)),
		validRuntimeProvider("desktop-vad", readiness.VoiceActivity, now.Add(time.Minute)),
	}
	if got := view.CurrentActivation(); got.Status != readiness.Degraded {
		t.Fatalf("activation with media and visual fallback = %#v, want DEGRADED", got)
	}

	withTTS, err := newActivationView(view.scenario, providers, clock, activationViewOptions{TTSEnabled: true})
	if err != nil {
		t.Fatalf("newActivationView(TTS) error = %v", err)
	}
	if got := withTTS.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("activation with media and TTS = %#v, want READY", got)
	}

	clock.Advance(time.Minute)
	if got := withTTS.CurrentActivation(); got.Status != readiness.Blocked {
		t.Fatalf("activation at media lease deadline = %#v, want BLOCKED", got)
	}
}

func TestActivationViewDisabledDoesNotRequireBiometricDependencies(t *testing.T) {
	now := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	view, err := newActivationView(readiness.ScenarioRequirements{
		ID:                       "anonymous-desktop",
		MinimumIdentityAssurance: readiness.IdentityAssuranceAnonymous,
		Required: []readiness.CapabilityRequirement{{
			Kind: readiness.DisplayText, ProviderID: "web-avatar",
			Compatibility: desktopCompatibility(100*time.Millisecond, readiness.ProviderDeviceDisplay),
		}},
	}, &snapshotReader{}, engineclock.NewFake(now), activationViewOptions{})
	if err != nil {
		t.Fatalf("newActivationView() error = %v", err)
	}

	if got := view.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("disabled CurrentActivation() = %#v, want READY without biometric dependencies", got)
	}
}

func TestActivationViewDisabledStillEvaluatesRequiredBiometricPolicy(t *testing.T) {
	now := time.Date(2026, 8, 15, 8, 15, 0, 0, time.UTC)
	providers := &snapshotReader{snapshots: []readiness.ProviderSnapshot{
		validRuntimeProvider("face-identity", readiness.FaceIdentification, now.Add(time.Minute)),
	}}
	view, err := newActivationView(faceIdentificationScenario(), providers, engineclock.NewFake(now), activationViewOptions{})
	if err != nil {
		t.Fatalf("newActivationView() error = %v", err)
	}

	got := view.CurrentActivation()
	if got.Status != readiness.Blocked || len(got.Issues) != 1 || got.Issues[0].Reason != readiness.IssueBiometricDisabled {
		t.Fatalf("disabled biometric CurrentActivation() = %#v, want BIOMETRIC_DISABLED", got)
	}
}

func TestActivationViewEnabledRejectsIncompleteBiometricReadiness(t *testing.T) {
	now := time.Date(2026, 8, 15, 8, 30, 0, 0, time.UTC)
	scenario := faceIdentificationScenario()
	providers := &snapshotReader{snapshots: []readiness.ProviderSnapshot{
		validRuntimeProvider("face-identity", readiness.FaceIdentification, now.Add(time.Minute)),
	}}
	privacyReader := &mutableActivationPrivacyReader{}
	catalog := &mutableBiometricReadinessCatalog{}
	var typedNilPrivacy *mutableActivationPrivacyReader
	var typedNilCatalog *mutableBiometricReadinessCatalog

	for _, test := range []struct {
		name     string
		identity *activationIdentityReadiness
	}{
		{name: "blank profile reference", identity: &activationIdentityReadiness{Privacy: privacyReader, Catalog: catalog}},
		{name: "profile reference with surrounding whitespace", identity: &activationIdentityReadiness{BiometricProfileRef: " profile-a ", Privacy: privacyReader, Catalog: catalog}},
		{name: "nil privacy reader", identity: &activationIdentityReadiness{BiometricProfileRef: "profile-a", Catalog: catalog}},
		{name: "typed nil privacy reader", identity: &activationIdentityReadiness{BiometricProfileRef: "profile-a", Privacy: typedNilPrivacy, Catalog: catalog}},
		{name: "nil catalog", identity: &activationIdentityReadiness{BiometricProfileRef: "profile-a", Privacy: privacyReader}},
		{name: "typed nil catalog", identity: &activationIdentityReadiness{BiometricProfileRef: "profile-a", Privacy: privacyReader, Catalog: typedNilCatalog}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newActivationView(scenario, providers, engineclock.NewFake(now), activationViewOptions{Identity: test.identity}); err == nil {
				t.Fatal("newActivationView() error = nil, want invalid biometric readiness configuration")
			}
		})
	}
}

func TestActivationViewUsesExplicitProfileAndLatestBiometricReadiness(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	privacyReader := &mutableActivationPrivacyReader{snapshot: faceIdentificationPermissions(1, true)}
	catalog := &mutableBiometricReadinessCatalog{
		expectedProfileRef: "biometric-profile-a",
		enrolled:           true,
	}
	view := newBiometricActivationView(t, now, privacyReader, catalog)

	if got := view.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("initial CurrentActivation() = %#v, want READY", got)
	}
	privacyReader.snapshot = faceIdentificationPermissions(2, false)
	if got := view.CurrentActivation(); got.Status != readiness.Blocked || len(got.Issues) != 1 || got.Issues[0].Reason != readiness.IssueBiometricUnauthorized {
		t.Fatalf("CurrentActivation() after permission revoke = %#v, want BIOMETRIC_UNAUTHORIZED", got)
	}
	privacyReader.snapshot = faceIdentificationPermissions(3, true)
	catalog.enrolled = false
	if got := view.CurrentActivation(); got.Status != readiness.Blocked || len(got.Issues) != 1 || got.Issues[0].Reason != readiness.IssueEnrollmentMissing {
		t.Fatalf("CurrentActivation() after enrollment removal = %#v, want ENROLLMENT_MISSING", got)
	}
	catalog.enrolled = true
	if got := view.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("CurrentActivation() after enrollment recovery = %#v, want READY", got)
	}

	if privacyReader.calls != 4 || catalog.calls != 4 {
		t.Fatalf("readiness calls = privacy %d, catalog %d, want 4 each", privacyReader.calls, catalog.calls)
	}
	for _, profileRef := range catalog.profileRefs {
		if profileRef != "biometric-profile-a" {
			t.Fatalf("ReadinessPolicy() profile reference = %q, want explicit biometric profile", profileRef)
		}
	}
}

func TestActivationViewBiometricReadFailuresAreNotCached(t *testing.T) {
	now := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	privacyReader := &mutableActivationPrivacyReader{snapshot: faceIdentificationPermissions(1, true)}
	catalog := &mutableBiometricReadinessCatalog{
		expectedProfileRef: "biometric-profile-a",
		enrolled:           true,
	}
	view := newBiometricActivationView(t, now, privacyReader, catalog)

	if got := view.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("initial CurrentActivation() = %#v, want READY", got)
	}
	privacyReader.err = errors.New("privacy unavailable")
	if got := view.CurrentActivation(); got.Status != readiness.Blocked || len(got.Issues) != 0 {
		t.Fatalf("CurrentActivation() privacy failure = %#v, want issue-free BLOCKED", got)
	}
	if catalog.calls != 1 {
		t.Fatalf("catalog calls after privacy failure = %d, want 1", catalog.calls)
	}
	privacyReader.err = nil
	catalog.err = errors.New("catalog unavailable")
	if got := view.CurrentActivation(); got.Status != readiness.Blocked || len(got.Issues) != 0 {
		t.Fatalf("CurrentActivation() catalog failure = %#v, want issue-free BLOCKED", got)
	}
	catalog.err = nil
	if got := view.CurrentActivation(); got.Status != readiness.Ready {
		t.Fatalf("CurrentActivation() after dependency recovery = %#v, want READY", got)
	}
	if privacyReader.calls != 4 || catalog.calls != 3 {
		t.Fatalf("readiness calls = privacy %d, catalog %d, want privacy 4 and catalog 3", privacyReader.calls, catalog.calls)
	}
}

type snapshotReader struct{ snapshots []readiness.ProviderSnapshot }

func (s *snapshotReader) Snapshots() []readiness.ProviderSnapshot {
	return cloneProviderSnapshots(s.snapshots)
}

func validRuntimeProvider(id string, capability readiness.CapabilityKind, expiresAt time.Time) readiness.ProviderSnapshot {
	profile := readiness.ProviderOperationalProfile{
		PrivacyClass: readiness.ProviderPrivacyDeviceLocal, MaximumLatency: time.Second,
		CancellationSemantics: readiness.ProviderCancellationCooperative,
	}
	switch capability {
	case readiness.PersonPresence:
		profile.MaximumLatency = 3 * time.Second
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceCamera}
	case readiness.FaceDetection, readiness.FaceIdentification, readiness.FaceLiveness:
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceCamera}
	case readiness.VoiceActivity:
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceMicrophone}
	case readiness.SpeakerIdentification, readiness.SpeakerVerification:
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceMicrophone}
	}
	return readiness.ProviderSnapshot{
		ProviderID: id, InstanceID: id + "-1", ProtocolVersion: "v1", ImplementationVersion: "test",
		Capabilities: []readiness.CapabilityKind{capability}, Health: readiness.Healthy, LeaseExpiresAt: expiresAt,
		OperationalProfile: profile,
	}
}

func faceIdentificationScenario() readiness.ScenarioRequirements {
	return readiness.ScenarioRequirements{
		ID:                       "face-identification",
		MinimumIdentityAssurance: readiness.IdentityAssuranceRecognized,
		Required: []readiness.CapabilityRequirement{{
			Kind: readiness.FaceIdentification, ProviderID: "face-identity",
			Compatibility: desktopCompatibility(time.Second, readiness.ProviderDeviceCamera),
		}},
	}
}

func newBiometricActivationView(
	t *testing.T,
	now time.Time,
	privacyReader *mutableActivationPrivacyReader,
	catalog *mutableBiometricReadinessCatalog,
) *activationView {
	t.Helper()
	providers := &snapshotReader{snapshots: []readiness.ProviderSnapshot{
		validRuntimeProvider("face-identity", readiness.FaceIdentification, now.Add(time.Minute)),
	}}
	view, err := newActivationView(faceIdentificationScenario(), providers, engineclock.NewFake(now), activationViewOptions{
		Identity: &activationIdentityReadiness{
			BiometricProfileRef: "biometric-profile-a",
			Privacy:             privacyReader,
			Catalog:             catalog,
		},
	})
	if err != nil {
		t.Fatalf("newActivationView() error = %v", err)
	}
	return view
}

type mutableActivationPrivacyReader struct {
	snapshot privacy.Snapshot
	err      error
	calls    int
}

func (r *mutableActivationPrivacyReader) Current(context.Context) (privacy.Snapshot, error) {
	r.calls++
	return r.snapshot, r.err
}

type mutableBiometricReadinessCatalog struct {
	expectedProfileRef string
	enrolled           bool
	err                error
	calls              int
	profileRefs        []string
}

func (c *mutableBiometricReadinessCatalog) ReadinessPolicy(snapshot privacy.Snapshot, profileRef string) (readiness.BiometricPolicySnapshot, error) {
	c.calls++
	c.profileRefs = append(c.profileRefs, profileRef)
	if c.err != nil {
		return readiness.BiometricPolicySnapshot{}, c.err
	}
	if profileRef != c.expectedProfileRef {
		return readiness.BiometricPolicySnapshot{}, errors.New("unexpected biometric profile reference")
	}
	policy := readiness.BiometricPolicySnapshot{Enabled: biometricRequested(snapshot)}
	if !permissionEnabled(snapshot, privacy.CameraCapture) ||
		!permissionEnabled(snapshot, privacy.FaceDetection) ||
		!permissionEnabled(snapshot, privacy.FaceIdentification) {
		return policy, nil
	}
	policy.Authorized = []readiness.CapabilityKind{readiness.FaceIdentification}
	if c.enrolled {
		policy.Enrolled = []readiness.CapabilityKind{readiness.FaceIdentification}
	}
	return policy, nil
}

func faceIdentificationPermissions(revision uint64, identificationEnabled bool) privacy.Snapshot {
	return privacy.Snapshot{Revision: revision, Grants: []privacy.Grant{
		{Permission: privacy.CameraCapture, Enabled: true},
		{Permission: privacy.FaceDetection, Enabled: true},
		{Permission: privacy.FaceIdentification, Enabled: identificationEnabled},
	}}
}

func permissionEnabled(snapshot privacy.Snapshot, permission privacy.Permission) bool {
	for _, grant := range snapshot.Grants {
		if grant.Permission == permission {
			return grant.Enabled
		}
	}
	return false
}

func biometricRequested(snapshot privacy.Snapshot) bool {
	for _, grant := range snapshot.Grants {
		switch grant.Permission {
		case privacy.FaceDetection, privacy.FaceIdentification, privacy.FaceLiveness,
			privacy.SpeakerIdentification, privacy.SpeakerVerification:
			if grant.Enabled {
				return true
			}
		}
	}
	return false
}

func desktopCompatibility(maximumLatency time.Duration, device readiness.ProviderDeviceClass) readiness.ProviderCompatibility {
	return readiness.ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        []readiness.ProviderPrivacyClass{readiness.ProviderPrivacyDeviceLocal},
		MaximumLatency:               maximumLatency,
		AllowedCancellationSemantics: []readiness.ProviderCancellationSemantics{readiness.ProviderCancellationCooperative},
		AllowedDeviceClasses:         []readiness.ProviderDeviceClass{device},
	}
}
