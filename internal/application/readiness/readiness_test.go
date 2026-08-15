package readiness

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

func TestEvaluateAtReadyWithExplicitHealthyProvider(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	scenario := ScenarioRequirements{
		ID:                       "anonymous-welcome",
		MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Required: []CapabilityRequirement{
			{Kind: PersonPresence, ProviderID: "camera-main", Compatibility: defaultCompatibility()},
			{Kind: VoiceActivity, ProviderID: "vad-main", Compatibility: defaultCompatibility()},
		},
	}
	providers := []ProviderSnapshot{
		provider("camera-main", now.Add(time.Minute), PersonPresence),
		provider("vad-main", now.Add(time.Minute), VoiceActivity),
	}

	got, err := EvaluateAt(scenario, providers, BiometricPolicySnapshot{}, now)
	if err != nil {
		t.Fatalf("EvaluateAt() error = %v", err)
	}
	if got.Status != Ready || len(got.Issues) != 0 {
		t.Fatalf("EvaluateAt() = %#v, want READY without issues", got)
	}
}

func TestValidateScenarioNeedsOnlyTheDeclaration(t *testing.T) {
	valid := []ScenarioRequirements{
		{
			ID:                       "required-only",
			MinimumIdentityAssurance: IdentityAssuranceAnonymous,
			Required:                 []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}},
		},
	}
	for _, optional := range legalOptionalCapabilities() {
		valid = append(valid, ScenarioRequirements{
			ID:                       "optional-" + string(optional.capability),
			MinimumIdentityAssurance: IdentityAssuranceAnonymous,
			Optional: []OptionalCapability{{
				Kind: optional.capability, ProviderID: "selected", Fallback: optional.fallback, Compatibility: defaultCompatibility(),
			}},
		})
	}
	for _, scenario := range valid {
		if err := ValidateScenario(scenario); err != nil {
			t.Fatalf("ValidateScenario(%q) error = %v", scenario.ID, err)
		}
	}

	invalid := []struct {
		name     string
		scenario ScenarioRequirements
	}{
		{name: "empty id", scenario: ScenarioRequirements{Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}}}},
		{name: "no capabilities", scenario: ScenarioRequirements{ID: "empty"}},
		{name: "duplicate required", scenario: ScenarioRequirements{ID: "duplicate", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}, {Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}}}},
		{name: "duplicate optional", scenario: ScenarioRequirements{ID: "duplicate", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply, Compatibility: defaultCompatibility()}, {Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply, Compatibility: defaultCompatibility()}}}},
		{name: "required optional overlap", scenario: ScenarioRequirements{ID: "overlap", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: VoiceActivity, ProviderID: "vad", Compatibility: defaultCompatibility()}}, Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply, Compatibility: defaultCompatibility()}}}},
		{name: "empty provider", scenario: ScenarioRequirements{ID: "provider", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: PersonPresence}}}},
		{name: "unknown capability", scenario: ScenarioRequirements{ID: "kind", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: CapabilityKind("UNKNOWN"), ProviderID: "provider", Compatibility: defaultCompatibility()}}}},
		{name: "unsupported optional", scenario: ScenarioRequirements{ID: "optional", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject, Compatibility: defaultCompatibility()}}}},
		{name: "mismatched fallback", scenario: ScenarioRequirements{ID: "fallback", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: SpeechSynthesis, ProviderID: "tts", Fallback: AudioOnly, Compatibility: defaultCompatibility()}}}},
		{name: "unknown fallback", scenario: ScenarioRequirements{ID: "fallback", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: Fallback("UNKNOWN"), Compatibility: defaultCompatibility()}}}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateScenario(test.scenario); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("ValidateScenario() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestEvaluateAtBlocksRequiredCapabilityFailuresWithStableCodes(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		providers  []ProviderSnapshot
		wantCode   fault.Code
		wantReason IssueReason
	}{
		{name: "selected provider missing", wantCode: fault.Unavailable, wantReason: IssueProviderMissing},
		{
			name:       "selected provider lacks capability",
			providers:  []ProviderSnapshot{provider("selected", now.Add(time.Minute), DeviceState)},
			wantCode:   fault.CapabilityMissing,
			wantReason: IssueCapabilityMissing,
		},
		{
			name: "selected provider unhealthy",
			providers: []ProviderSnapshot{func() ProviderSnapshot {
				p := provider("selected", now.Add(time.Minute), PersonPresence)
				p.Health = Unhealthy
				return p
			}()},
			wantCode:   fault.Unavailable,
			wantReason: IssueProviderUnhealthy,
		},
		{
			name:       "lease expires exactly now",
			providers:  []ProviderSnapshot{provider("selected", now, PersonPresence)},
			wantCode:   fault.DeadlineExceeded,
			wantReason: IssueLeaseExpired,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := ScenarioRequirements{
				ID:                       "required-presence",
				MinimumIdentityAssurance: IdentityAssuranceAnonymous,
				Required:                 []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "selected", Compatibility: defaultCompatibility()}},
			}
			got, err := EvaluateAt(scenario, test.providers, BiometricPolicySnapshot{}, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			wantIssues := []Issue{{Capability: PersonPresence, ProviderID: "selected", Code: test.wantCode, Reason: test.wantReason}}
			if got.Status != Blocked || !reflect.DeepEqual(got.Issues, wantIssues) {
				t.Fatalf("EvaluateAt() = %#v, want BLOCKED with %#v", got, wantIssues)
			}
		})
	}
}

func TestEvaluateAtDegradesOptionalCapabilitiesOnlyWithDeclaredLegalFallback(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)

	for _, test := range legalOptionalCapabilities() {
		t.Run(string(test.capability), func(t *testing.T) {
			scenario := ScenarioRequirements{
				ID:                       "optional-degradation",
				MinimumIdentityAssurance: IdentityAssuranceAnonymous,
				Optional: []OptionalCapability{{
					Kind:          test.capability,
					ProviderID:    "selected",
					Fallback:      test.fallback,
					Compatibility: defaultCompatibility(),
				}},
			}
			got, err := EvaluateAt(scenario, nil, biometricAccess(test.capability), now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			wantIssues := []Issue{{
				Capability: test.capability,
				ProviderID: "selected",
				Code:       fault.Unavailable,
				Reason:     IssueProviderMissing,
				Fallback:   test.fallback,
			}}
			if got.Status != Degraded || !reflect.DeepEqual(got.Issues, wantIssues) {
				t.Fatalf("EvaluateAt() = %#v, want DEGRADED with %#v", got, wantIssues)
			}
		})
	}
}

func TestEvaluateAtRejectsInvalidDeclarations(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	validRequired := CapabilityRequirement{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}
	validScenario := ScenarioRequirements{ID: "valid", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{validRequired}}
	validProvider := provider("camera", now.Add(time.Minute), PersonPresence)
	emptyInstance := validProvider
	emptyInstance.InstanceID = ""
	emptyProtocol := validProvider
	emptyProtocol.ProtocolVersion = ""
	emptyImplementation := validProvider
	emptyImplementation.ImplementationVersion = ""
	emptyLease := validProvider
	emptyLease.LeaseExpiresAt = time.Time{}
	unknownHealth := validProvider
	unknownHealth.Health = "UNKNOWN"
	tests := []struct {
		name      string
		scenario  ScenarioRequirements
		providers []ProviderSnapshot
		policy    BiometricPolicySnapshot
	}{
		{
			name:     "empty scenario id",
			scenario: ScenarioRequirements{Required: []CapabilityRequirement{validRequired}},
		},
		{
			name:     "scenario without capabilities",
			scenario: ScenarioRequirements{ID: "empty"},
		},
		{
			name:     "duplicate required capability",
			scenario: ScenarioRequirements{ID: "duplicate", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{validRequired, validRequired}},
		},
		{
			name: "duplicate optional capability",
			scenario: ScenarioRequirements{ID: "duplicate-optional", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{
				{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply, Compatibility: defaultCompatibility()},
				{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply, Compatibility: defaultCompatibility()},
			}},
		},
		{
			name: "required optional overlap",
			scenario: ScenarioRequirements{
				ID:                       "overlap",
				MinimumIdentityAssurance: IdentityAssuranceAnonymous,
				Required:                 []CapabilityRequirement{validRequired},
				Optional:                 []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject, Compatibility: defaultCompatibility()}},
			},
		},
		{
			name:     "empty selected provider",
			scenario: ScenarioRequirements{ID: "empty-provider", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: PersonPresence}}},
		},
		{
			name:     "unknown capability",
			scenario: ScenarioRequirements{ID: "unknown-kind", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: CapabilityKind("UNKNOWN"), ProviderID: "camera", Compatibility: defaultCompatibility()}}},
		},
		{
			name:     "unsupported optional capability",
			scenario: ScenarioRequirements{ID: "unsupported-optional", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject, Compatibility: defaultCompatibility()}}},
		},
		{
			name:     "mismatched fallback",
			scenario: ScenarioRequirements{ID: "mismatched-fallback", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: SpeechSynthesis, ProviderID: "tts", Fallback: AudioOnly, Compatibility: defaultCompatibility()}}},
		},
		{
			name:     "unknown fallback",
			scenario: ScenarioRequirements{ID: "unknown-fallback", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: Fallback("UNKNOWN"), Compatibility: defaultCompatibility()}}},
		},
		{
			name:      "duplicate provider snapshot",
			scenario:  ScenarioRequirements{ID: "duplicate-provider", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{validRequired}},
			providers: []ProviderSnapshot{provider("camera", now.Add(time.Minute), PersonPresence), provider("camera", now.Add(time.Minute), PersonPresence)},
		},
		{
			name:     "duplicate provider capability",
			scenario: ScenarioRequirements{ID: "duplicate-provider-capability", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{validRequired}},
			providers: []ProviderSnapshot{
				provider("camera", now.Add(time.Minute), PersonPresence, PersonPresence),
			},
		},
		{name: "empty provider instance id", scenario: validScenario, providers: []ProviderSnapshot{emptyInstance}},
		{name: "empty provider protocol version", scenario: validScenario, providers: []ProviderSnapshot{emptyProtocol}},
		{name: "empty provider implementation version", scenario: validScenario, providers: []ProviderSnapshot{emptyImplementation}},
		{name: "empty provider lease", scenario: validScenario, providers: []ProviderSnapshot{emptyLease}},
		{name: "unknown provider health", scenario: validScenario, providers: []ProviderSnapshot{unknownHealth}},
		{
			name:      "unknown provider capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{provider("camera", now.Add(time.Minute), CapabilityKind("UNKNOWN"))},
		},
		{
			name:      "empty provider capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{provider("camera", now.Add(time.Minute), CapabilityKind(""))},
		},
		{
			name:      "provider without capabilities",
			scenario:  validScenario,
			providers: []ProviderSnapshot{provider("camera", now.Add(time.Minute))},
		},
		{
			name:      "authorized contains non-biometric capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{PersonPresence}},
		},
		{
			name:      "authorized contains unknown capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{CapabilityKind("UNKNOWN")}},
		},
		{
			name:      "authorized contains duplicate capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{FaceIdentification, FaceIdentification}},
		},
		{
			name:      "enrolled contains non-biometric capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Enrolled: []CapabilityKind{PersonPresence}},
		},
		{
			name:      "enrolled contains unknown capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Enrolled: []CapabilityKind{CapabilityKind("UNKNOWN")}},
		},
		{
			name:      "enrolled contains duplicate capability",
			scenario:  validScenario,
			providers: []ProviderSnapshot{validProvider},
			policy:    BiometricPolicySnapshot{Enabled: true, Enrolled: []CapabilityKind{SpeakerIdentification, SpeakerIdentification}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := EvaluateAt(test.scenario, test.providers, test.policy, now)
			if !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("EvaluateAt() error = %v, want InvalidInput", err)
			}
		})
	}
}

func TestEvaluateAtIsIndependentOfInputOrdering(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	presence := provider("camera", now.Add(time.Minute), PersonPresence)
	presence.Health = Unhealthy
	voice := provider("vad", now.Add(time.Minute), VoiceActivity)
	voice.Health = Unhealthy

	first, err := EvaluateAt(
		ScenarioRequirements{ID: "stable", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{
			{Kind: VoiceActivity, ProviderID: "vad", Compatibility: defaultCompatibility()},
			{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()},
		}},
		[]ProviderSnapshot{voice, presence}, BiometricPolicySnapshot{}, now,
	)
	if err != nil {
		t.Fatalf("first EvaluateAt() error = %v", err)
	}
	second, err := EvaluateAt(
		ScenarioRequirements{ID: "stable", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{
			{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()},
			{Kind: VoiceActivity, ProviderID: "vad", Compatibility: defaultCompatibility()},
		}},
		[]ProviderSnapshot{presence, voice}, BiometricPolicySnapshot{}, now,
	)
	if err != nil {
		t.Fatalf("second EvaluateAt() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("EvaluateAt() depends on ordering: first %#v, second %#v", first, second)
	}
	if !sort.SliceIsSorted(first.Issues, func(i, j int) bool {
		left := string(first.Issues[i].Capability) + "\x00" + first.Issues[i].ProviderID
		right := string(first.Issues[j].Capability) + "\x00" + first.Issues[j].ProviderID
		return left < right
	}) {
		t.Fatalf("issues are not stably sorted: %#v", first.Issues)
	}
}

func TestEvaluateAtEnforcesBiometricPolicyWithoutBlockingNonBiometrics(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	face := provider("face", now.Add(time.Minute), FaceIdentification)
	scenario := ScenarioRequirements{
		ID:                       "personalized",
		MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Required:                 []CapabilityRequirement{{Kind: FaceIdentification, ProviderID: "face", Compatibility: defaultCompatibility()}},
	}
	tests := []struct {
		name       string
		policy     BiometricPolicySnapshot
		wantCode   fault.Code
		wantReason IssueReason
	}{
		{name: "disabled by default", wantCode: fault.PolicyBlocked, wantReason: IssueBiometricDisabled},
		{
			name:       "not authorized",
			policy:     BiometricPolicySnapshot{Enabled: true, Enrolled: []CapabilityKind{FaceIdentification}},
			wantCode:   fault.PermissionDenied,
			wantReason: IssueBiometricUnauthorized,
		},
		{
			name:       "not enrolled",
			policy:     BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{FaceIdentification}},
			wantCode:   fault.PolicyBlocked,
			wantReason: IssueEnrollmentMissing,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateAt(scenario, []ProviderSnapshot{face}, test.policy, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			want := []Issue{{Capability: FaceIdentification, ProviderID: "face", Code: test.wantCode, Reason: test.wantReason}}
			if got.Status != Blocked || !reflect.DeepEqual(got.Issues, want) {
				t.Fatalf("EvaluateAt() = %#v, want BLOCKED with %#v", got, want)
			}
		})
	}

	nonBiometric, err := EvaluateAt(
		ScenarioRequirements{ID: "anonymous", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}}},
		[]ProviderSnapshot{provider("camera", now.Add(time.Minute), PersonPresence)},
		BiometricPolicySnapshot{}, now,
	)
	if err != nil || nonBiometric.Status != Ready {
		t.Fatalf("non-biometric EvaluateAt() = %#v, %v, want READY", nonBiometric, err)
	}

	for _, capability := range []CapabilityKind{FaceDetection, FaceLiveness} {
		scenario := ScenarioRequirements{ID: "biometric-auxiliary", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: capability, ProviderID: "face", Compatibility: defaultCompatibility()}}}
		providers := []ProviderSnapshot{provider("face", now.Add(time.Minute), capability)}
		blocked, err := EvaluateAt(scenario, providers, BiometricPolicySnapshot{}, now)
		if err != nil || blocked.Status != Blocked {
			t.Fatalf("unauthorized %q = %#v, %v, want BLOCKED", capability, blocked, err)
		}
		got, err := EvaluateAt(
			scenario, providers,
			BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{capability}}, now,
		)
		if err != nil || got.Status != Ready {
			t.Fatalf("authorized %q without enrollment = %#v, %v, want READY", capability, got, err)
		}
	}
}

func TestEvaluateAtDegradesUnauthorizedOptionalBiometricToAnonymous(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	got, err := EvaluateAt(
		ScenarioRequirements{ID: "anonymous-fallback", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Optional: []OptionalCapability{{
			Kind: FaceIdentification, ProviderID: "face", Fallback: AnonymousSubject, Compatibility: defaultCompatibility(),
		}}},
		[]ProviderSnapshot{provider("face", now.Add(time.Minute), FaceIdentification)},
		BiometricPolicySnapshot{}, now,
	)
	if err != nil {
		t.Fatalf("EvaluateAt() error = %v", err)
	}
	want := []Issue{{
		Capability: FaceIdentification,
		ProviderID: "face",
		Code:       fault.PolicyBlocked,
		Reason:     IssueBiometricDisabled,
		Fallback:   AnonymousSubject,
	}}
	if got.Status != Degraded || !reflect.DeepEqual(got.Issues, want) {
		t.Fatalf("EvaluateAt() = %#v, want DEGRADED with %#v", got, want)
	}
}

func TestBiometricIdentificationLivenessAndVerificationAreIndependentKinds(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		required   CapabilityKind
		different  CapabilityKind
		providerID string
	}{
		{name: "face identification does not authorize liveness", required: FaceLiveness, different: FaceIdentification, providerID: "face"},
		{name: "speaker identification does not authorize verification", required: SpeakerVerification, different: SpeakerIdentification, providerID: "speaker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateAt(
				ScenarioRequirements{ID: "independent", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: test.required, ProviderID: test.providerID, Compatibility: defaultCompatibility()}}},
				[]ProviderSnapshot{provider(test.providerID, now.Add(time.Minute), test.required)},
				BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{test.different}, Enrolled: []CapabilityKind{test.different}},
				now,
			)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			if got.Status != Blocked || len(got.Issues) != 1 || got.Issues[0].Capability != test.required {
				t.Fatalf("EvaluateAt() = %#v, want required kind independently blocked", got)
			}
		})
	}
}

func TestCapabilityCatalogCoversProductRequirements(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	capabilities := []CapabilityKind{
		PersonPresence, FaceDetection, FaceIdentification, FaceLiveness,
		VoiceActivity, SpeakerIdentification, SpeakerVerification,
		SpeechTranscription, AttentionEstimation, BusyState, GestureDetection,
		DeviceState, DisplayText, AvatarAttend, AvatarExpression,
		SpeechSynthesis, Gesture, Light, Locomotion,
	}
	seen := make(map[CapabilityKind]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" {
			t.Fatal("capability catalog contains an empty kind")
		}
		if _, exists := seen[capability]; exists {
			t.Fatalf("capability catalog contains duplicate %q", capability)
		}
		seen[capability] = struct{}{}

		got, err := EvaluateAt(
			ScenarioRequirements{ID: "catalog", MinimumIdentityAssurance: IdentityAssuranceAnonymous, Required: []CapabilityRequirement{{Kind: capability, ProviderID: "provider", Compatibility: defaultCompatibility()}}},
			[]ProviderSnapshot{provider("provider", now.Add(time.Minute), capability)},
			biometricAccess(capability), now,
		)
		if err != nil || got.Status != Ready {
			t.Fatalf("capability %q is not accepted as a real catalog kind: got %#v, error %v", capability, got, err)
		}
	}
}

func provider(id string, leaseExpiresAt time.Time, capabilities ...CapabilityKind) ProviderSnapshot {
	return ProviderSnapshot{
		ProviderID:            id,
		InstanceID:            id + "-instance",
		ProtocolVersion:       "v1",
		ImplementationVersion: "test-v1",
		Capabilities:          capabilities,
		Health:                Healthy,
		LeaseExpiresAt:        leaseExpiresAt,
		OperationalProfile: ProviderOperationalProfile{
			PrivacyClass:          ProviderPrivacyDeviceLocal,
			MaximumLatency:        50 * time.Millisecond,
			CancellationSemantics: ProviderCancellationCooperative,
		},
	}
}

func biometricAccess(capability CapabilityKind) BiometricPolicySnapshot {
	switch capability {
	case FaceDetection, FaceLiveness:
		return BiometricPolicySnapshot{
			Enabled:    true,
			Authorized: []CapabilityKind{capability},
		}
	case FaceIdentification, SpeakerIdentification, SpeakerVerification:
		return BiometricPolicySnapshot{
			Enabled:    true,
			Authorized: []CapabilityKind{capability},
			Enrolled:   []CapabilityKind{capability},
		}
	default:
		return BiometricPolicySnapshot{}
	}
}

func legalOptionalCapabilities() []struct {
	capability CapabilityKind
	fallback   Fallback
} {
	return []struct {
		capability CapabilityKind
		fallback   Fallback
	}{
		{FaceDetection, AnonymousSubject},
		{FaceIdentification, AnonymousSubject},
		{FaceLiveness, AnonymousSubject},
		{SpeakerIdentification, AnonymousSubject},
		{SpeakerVerification, AnonymousSubject},
		{VoiceActivity, NoVoiceReply},
		{SpeechTranscription, NoTranscript},
		{SpeechSynthesis, VisualOnly},
		{DisplayText, AudioOnly},
		{AvatarAttend, AudioOnly},
		{AvatarExpression, AudioOnly},
	}
}

func TestEvaluateAtMatchesSelectedProviderCompatibility(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	compatible := compatibility(
		[]ProviderPrivacyClass{ProviderPrivacyDeviceLocal},
		100*time.Millisecond,
		[]ProviderCancellationSemantics{ProviderCancellationCooperative},
		[]ProviderDeviceClass{ProviderDeviceCamera, ProviderDeviceMicrophone},
	)
	scenario := ScenarioRequirements{
		ID:                       "compatibility",
		MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Required: []CapabilityRequirement{{
			Kind: PersonPresence, ProviderID: "selected", Compatibility: compatible,
		}},
	}
	baseline := provider("selected", now.Add(time.Minute), PersonPresence)
	baseline.OperationalProfile = ProviderOperationalProfile{
		PrivacyClass:          ProviderPrivacyDeviceLocal,
		MaximumLatency:        50 * time.Millisecond,
		CancellationSemantics: ProviderCancellationCooperative,
		DeviceRequirements:    []ProviderDeviceClass{ProviderDeviceCamera},
	}

	ready, err := EvaluateAt(scenario, []ProviderSnapshot{baseline}, BiometricPolicySnapshot{}, now)
	if err != nil || ready.Status != Ready {
		t.Fatalf("compatible EvaluateAt() = %#v, %v, want READY", ready, err)
	}

	tests := []struct {
		name   string
		mutate func(*ProviderSnapshot)
		code   fault.Code
		reason IssueReason
	}{
		{name: "protocol", mutate: func(value *ProviderSnapshot) { value.ProtocolVersion = "v2" }, code: fault.CapabilityMissing, reason: IssueProtocolIncompatible},
		{name: "privacy", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.PrivacyClass = ProviderPrivacyRemoteProcessing }, code: fault.PolicyBlocked, reason: IssuePrivacyIncompatible},
		{name: "latency", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.MaximumLatency = 101 * time.Millisecond }, code: fault.DeadlineExceeded, reason: IssueLatencyIncompatible},
		{name: "cancellation", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.CancellationSemantics = ProviderCancellationBounded
		}, code: fault.CapabilityMissing, reason: IssueCancellationIncompatible},
		{name: "devices", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.DeviceRequirements = []ProviderDeviceClass{ProviderDeviceCamera, ProviderDeviceDisplay}
		}, code: fault.CapabilityMissing, reason: IssueDeviceIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := baseline
			test.mutate(&selected)
			got, evaluateErr := EvaluateAt(scenario, []ProviderSnapshot{selected}, BiometricPolicySnapshot{}, now)
			if evaluateErr != nil {
				t.Fatalf("EvaluateAt() error = %v", evaluateErr)
			}
			want := []Issue{{
				Capability: PersonPresence, ProviderID: "selected", Code: test.code, Reason: test.reason,
			}}
			if got.Status != Blocked || !reflect.DeepEqual(got.Issues, want) {
				t.Fatalf("EvaluateAt() = %#v, want BLOCKED with %#v", got, want)
			}
		})
	}
}

func TestEvaluateAtDegradesOptionalCompatibilityMismatch(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	scenario := ScenarioRequirements{
		ID:                       "optional-compatibility",
		MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Optional: []OptionalCapability{{
			Kind: SpeechSynthesis, ProviderID: "tts", Fallback: VisualOnly,
			Compatibility: compatibility(
				[]ProviderPrivacyClass{ProviderPrivacyDeviceLocal},
				100*time.Millisecond,
				[]ProviderCancellationSemantics{ProviderCancellationBounded},
				[]ProviderDeviceClass{ProviderDeviceAudioOutput},
			),
		}},
	}
	selected := provider("tts", now.Add(time.Minute), SpeechSynthesis)
	selected.OperationalProfile = ProviderOperationalProfile{
		PrivacyClass:          ProviderPrivacyDeviceLocal,
		MaximumLatency:        50 * time.Millisecond,
		CancellationSemantics: ProviderCancellationCooperative,
		DeviceRequirements:    []ProviderDeviceClass{ProviderDeviceAudioOutput},
	}

	got, err := EvaluateAt(scenario, []ProviderSnapshot{selected}, BiometricPolicySnapshot{}, now)
	if err != nil {
		t.Fatalf("EvaluateAt() error = %v", err)
	}
	want := []Issue{{
		Capability: SpeechSynthesis, ProviderID: "tts", Code: fault.CapabilityMissing,
		Reason: IssueCancellationIncompatible, Fallback: VisualOnly,
	}}
	if got.Status != Degraded || !reflect.DeepEqual(got.Issues, want) {
		t.Fatalf("EvaluateAt() = %#v, want DEGRADED with %#v", got, want)
	}
}

func TestValidateScenarioRequiresCompatibilityAndStaticIdentityAssurance(t *testing.T) {
	validCompatibility := compatibility(
		[]ProviderPrivacyClass{ProviderPrivacyDeviceLocal},
		time.Second,
		[]ProviderCancellationSemantics{ProviderCancellationCooperative},
		nil,
	)
	valid := ScenarioRequirements{
		ID:                       "anonymous",
		MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Required:                 []CapabilityRequirement{{Kind: DisplayText, ProviderID: "display", Compatibility: validCompatibility}},
	}
	if err := ValidateScenario(valid); err != nil {
		t.Fatalf("ValidateScenario(valid) error = %v", err)
	}

	tests := []struct {
		name     string
		scenario ScenarioRequirements
	}{
		{name: "missing assurance", scenario: func() ScenarioRequirements {
			value := valid
			value.MinimumIdentityAssurance = ""
			return value
		}()},
		{name: "recognized without identity", scenario: func() ScenarioRequirements {
			value := valid
			value.MinimumIdentityAssurance = IdentityAssuranceRecognized
			return value
		}()},
		{name: "verified without verification", scenario: ScenarioRequirements{
			ID: "verified", MinimumIdentityAssurance: IdentityAssuranceVerified,
			Optional: []OptionalCapability{{
				Kind: FaceIdentification, ProviderID: "face", Fallback: AnonymousSubject, Compatibility: validCompatibility,
			}},
		}},
		{name: "missing compatibility", scenario: ScenarioRequirements{
			ID: "missing-compatibility", MinimumIdentityAssurance: IdentityAssuranceAnonymous,
			Required: []CapabilityRequirement{{Kind: DisplayText, ProviderID: "display"}},
		}},
		{name: "empty privacy allowlist", scenario: ScenarioRequirements{
			ID: "privacy", MinimumIdentityAssurance: IdentityAssuranceAnonymous,
			Required: []CapabilityRequirement{{
				Kind: DisplayText, ProviderID: "display",
				Compatibility: compatibility(nil, time.Second, []ProviderCancellationSemantics{ProviderCancellationCooperative}, nil),
			}},
		}},
		{name: "duplicate device class", scenario: ScenarioRequirements{
			ID: "devices", MinimumIdentityAssurance: IdentityAssuranceAnonymous,
			Required: []CapabilityRequirement{{
				Kind: DisplayText, ProviderID: "display",
				Compatibility: compatibility(
					[]ProviderPrivacyClass{ProviderPrivacyDeviceLocal}, time.Second,
					[]ProviderCancellationSemantics{ProviderCancellationCooperative},
					[]ProviderDeviceClass{ProviderDeviceDisplay, ProviderDeviceDisplay},
				),
			}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateScenario(test.scenario); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("ValidateScenario() error = %v, want InvalidInput", err)
			}
		})
	}

	for _, assurance := range []IdentityAssurance{IdentityAssuranceRecognized, IdentityAssuranceVerified} {
		capability := FaceIdentification
		if assurance == IdentityAssuranceVerified {
			capability = SpeakerVerification
		}
		scenario := ScenarioRequirements{
			ID: "identity-" + string(assurance), MinimumIdentityAssurance: assurance,
			Optional: []OptionalCapability{{
				Kind: capability, ProviderID: "identity", Fallback: AnonymousSubject, Compatibility: validCompatibility,
			}},
		}
		if err := ValidateScenario(scenario); err != nil {
			t.Fatalf("ValidateScenario(%s with anonymous fallback) error = %v", assurance, err)
		}
	}
}

func TestEvaluateAtRejectsInvalidProviderOperationalProfiles(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	scenario := ScenarioRequirements{
		ID: "provider-profile", MinimumIdentityAssurance: IdentityAssuranceAnonymous,
		Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera", Compatibility: defaultCompatibility()}},
	}
	valid := provider("camera", now.Add(time.Minute), PersonPresence)
	tests := []struct {
		name   string
		mutate func(*ProviderSnapshot)
	}{
		{name: "zero profile", mutate: func(value *ProviderSnapshot) { value.OperationalProfile = ProviderOperationalProfile{} }},
		{name: "unknown privacy", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.PrivacyClass = "UNKNOWN" }},
		{name: "zero latency", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.MaximumLatency = 0 }},
		{name: "unknown cancellation", mutate: func(value *ProviderSnapshot) { value.OperationalProfile.CancellationSemantics = "UNKNOWN" }},
		{name: "unknown device", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.DeviceRequirements = []ProviderDeviceClass{"UNKNOWN"}
		}},
		{name: "duplicate device", mutate: func(value *ProviderSnapshot) {
			value.OperationalProfile.DeviceRequirements = []ProviderDeviceClass{ProviderDeviceCamera, ProviderDeviceCamera}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected := valid
			test.mutate(&selected)
			if _, err := EvaluateAt(scenario, []ProviderSnapshot{selected}, BiometricPolicySnapshot{}, now); !fault.IsCode(err, fault.InvalidInput) {
				t.Fatalf("EvaluateAt() error = %v, want InvalidInput", err)
			}
		})
	}
}

func compatibility(
	privacyClasses []ProviderPrivacyClass,
	maximumLatency time.Duration,
	cancellation []ProviderCancellationSemantics,
	devices []ProviderDeviceClass,
) ProviderCompatibility {
	return ProviderCompatibility{
		ProtocolVersion:              "v1",
		AllowedPrivacyClasses:        privacyClasses,
		MaximumLatency:               maximumLatency,
		AllowedCancellationSemantics: cancellation,
		AllowedDeviceClasses:         devices,
	}
}

func defaultCompatibility() ProviderCompatibility {
	return compatibility(
		[]ProviderPrivacyClass{ProviderPrivacyDeviceLocal},
		time.Second,
		[]ProviderCancellationSemantics{ProviderCancellationCooperative},
		[]ProviderDeviceClass{ProviderDeviceCamera},
	)
}
