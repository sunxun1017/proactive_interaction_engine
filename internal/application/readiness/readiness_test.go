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
		ID: "anonymous-welcome",
		Required: []CapabilityRequirement{
			{Kind: PersonPresence, ProviderID: "camera-main"},
			{Kind: VoiceActivity, ProviderID: "vad-main"},
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
			ID:       "required-only",
			Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera"}},
		},
	}
	for _, optional := range legalOptionalCapabilities() {
		valid = append(valid, ScenarioRequirements{
			ID: "optional-" + string(optional.capability),
			Optional: []OptionalCapability{{
				Kind: optional.capability, ProviderID: "selected", Fallback: optional.fallback,
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
		{name: "empty id", scenario: ScenarioRequirements{Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera"}}}},
		{name: "no capabilities", scenario: ScenarioRequirements{ID: "empty"}},
		{name: "duplicate required", scenario: ScenarioRequirements{ID: "duplicate", Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera"}, {Kind: PersonPresence, ProviderID: "camera"}}}},
		{name: "duplicate optional", scenario: ScenarioRequirements{ID: "duplicate", Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply}, {Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply}}}},
		{name: "required optional overlap", scenario: ScenarioRequirements{ID: "overlap", Required: []CapabilityRequirement{{Kind: VoiceActivity, ProviderID: "vad"}}, Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply}}}},
		{name: "empty provider", scenario: ScenarioRequirements{ID: "provider", Required: []CapabilityRequirement{{Kind: PersonPresence}}}},
		{name: "unknown capability", scenario: ScenarioRequirements{ID: "kind", Required: []CapabilityRequirement{{Kind: CapabilityKind("UNKNOWN"), ProviderID: "provider"}}}},
		{name: "unsupported optional", scenario: ScenarioRequirements{ID: "optional", Optional: []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject}}}},
		{name: "mismatched fallback", scenario: ScenarioRequirements{ID: "fallback", Optional: []OptionalCapability{{Kind: SpeechSynthesis, ProviderID: "tts", Fallback: AudioOnly}}}},
		{name: "unknown fallback", scenario: ScenarioRequirements{ID: "fallback", Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: Fallback("UNKNOWN")}}}},
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
		name      string
		providers []ProviderSnapshot
		wantCode  fault.Code
	}{
		{name: "selected provider missing", wantCode: fault.Unavailable},
		{
			name:      "selected provider lacks capability",
			providers: []ProviderSnapshot{provider("selected", now.Add(time.Minute), DeviceState)},
			wantCode:  fault.CapabilityMissing,
		},
		{
			name: "selected provider unhealthy",
			providers: []ProviderSnapshot{func() ProviderSnapshot {
				p := provider("selected", now.Add(time.Minute), PersonPresence)
				p.Health = Unhealthy
				return p
			}()},
			wantCode: fault.Unavailable,
		},
		{
			name:      "lease expires exactly now",
			providers: []ProviderSnapshot{provider("selected", now, PersonPresence)},
			wantCode:  fault.DeadlineExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := ScenarioRequirements{
				ID:       "required-presence",
				Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "selected"}},
			}
			got, err := EvaluateAt(scenario, test.providers, BiometricPolicySnapshot{}, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			wantIssues := []Issue{{Capability: PersonPresence, ProviderID: "selected", Code: test.wantCode}}
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
				ID: "optional-degradation",
				Optional: []OptionalCapability{{
					Kind:       test.capability,
					ProviderID: "selected",
					Fallback:   test.fallback,
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
	validRequired := CapabilityRequirement{Kind: PersonPresence, ProviderID: "camera"}
	validScenario := ScenarioRequirements{ID: "valid", Required: []CapabilityRequirement{validRequired}}
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
			scenario: ScenarioRequirements{ID: "duplicate", Required: []CapabilityRequirement{validRequired, validRequired}},
		},
		{
			name: "duplicate optional capability",
			scenario: ScenarioRequirements{ID: "duplicate-optional", Optional: []OptionalCapability{
				{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply},
				{Kind: VoiceActivity, ProviderID: "vad", Fallback: NoVoiceReply},
			}},
		},
		{
			name: "required optional overlap",
			scenario: ScenarioRequirements{
				ID:       "overlap",
				Required: []CapabilityRequirement{validRequired},
				Optional: []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject}},
			},
		},
		{
			name:     "empty selected provider",
			scenario: ScenarioRequirements{ID: "empty-provider", Required: []CapabilityRequirement{{Kind: PersonPresence}}},
		},
		{
			name:     "unknown capability",
			scenario: ScenarioRequirements{ID: "unknown-kind", Required: []CapabilityRequirement{{Kind: CapabilityKind("UNKNOWN"), ProviderID: "camera"}}},
		},
		{
			name:     "unsupported optional capability",
			scenario: ScenarioRequirements{ID: "unsupported-optional", Optional: []OptionalCapability{{Kind: PersonPresence, ProviderID: "camera", Fallback: AnonymousSubject}}},
		},
		{
			name:     "mismatched fallback",
			scenario: ScenarioRequirements{ID: "mismatched-fallback", Optional: []OptionalCapability{{Kind: SpeechSynthesis, ProviderID: "tts", Fallback: AudioOnly}}},
		},
		{
			name:     "unknown fallback",
			scenario: ScenarioRequirements{ID: "unknown-fallback", Optional: []OptionalCapability{{Kind: VoiceActivity, ProviderID: "vad", Fallback: Fallback("UNKNOWN")}}},
		},
		{
			name:      "duplicate provider snapshot",
			scenario:  ScenarioRequirements{ID: "duplicate-provider", Required: []CapabilityRequirement{validRequired}},
			providers: []ProviderSnapshot{provider("camera", now.Add(time.Minute), PersonPresence), provider("camera", now.Add(time.Minute), PersonPresence)},
		},
		{
			name:     "duplicate provider capability",
			scenario: ScenarioRequirements{ID: "duplicate-provider-capability", Required: []CapabilityRequirement{validRequired}},
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
		ScenarioRequirements{ID: "stable", Required: []CapabilityRequirement{
			{Kind: VoiceActivity, ProviderID: "vad"},
			{Kind: PersonPresence, ProviderID: "camera"},
		}},
		[]ProviderSnapshot{voice, presence}, BiometricPolicySnapshot{}, now,
	)
	if err != nil {
		t.Fatalf("first EvaluateAt() error = %v", err)
	}
	second, err := EvaluateAt(
		ScenarioRequirements{ID: "stable", Required: []CapabilityRequirement{
			{Kind: PersonPresence, ProviderID: "camera"},
			{Kind: VoiceActivity, ProviderID: "vad"},
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
		ID:       "personalized",
		Required: []CapabilityRequirement{{Kind: FaceIdentification, ProviderID: "face"}},
	}
	tests := []struct {
		name     string
		policy   BiometricPolicySnapshot
		wantCode fault.Code
	}{
		{name: "disabled by default", wantCode: fault.PolicyBlocked},
		{
			name:     "not authorized",
			policy:   BiometricPolicySnapshot{Enabled: true, Enrolled: []CapabilityKind{FaceIdentification}},
			wantCode: fault.PermissionDenied,
		},
		{
			name:     "not enrolled",
			policy:   BiometricPolicySnapshot{Enabled: true, Authorized: []CapabilityKind{FaceIdentification}},
			wantCode: fault.PolicyBlocked,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateAt(scenario, []ProviderSnapshot{face}, test.policy, now)
			if err != nil {
				t.Fatalf("EvaluateAt() error = %v", err)
			}
			want := []Issue{{Capability: FaceIdentification, ProviderID: "face", Code: test.wantCode}}
			if got.Status != Blocked || !reflect.DeepEqual(got.Issues, want) {
				t.Fatalf("EvaluateAt() = %#v, want BLOCKED with %#v", got, want)
			}
		})
	}

	nonBiometric, err := EvaluateAt(
		ScenarioRequirements{ID: "anonymous", Required: []CapabilityRequirement{{Kind: PersonPresence, ProviderID: "camera"}}},
		[]ProviderSnapshot{provider("camera", now.Add(time.Minute), PersonPresence)},
		BiometricPolicySnapshot{}, now,
	)
	if err != nil || nonBiometric.Status != Ready {
		t.Fatalf("non-biometric EvaluateAt() = %#v, %v, want READY", nonBiometric, err)
	}

	for _, capability := range []CapabilityKind{FaceDetection, FaceLiveness} {
		scenario := ScenarioRequirements{ID: "biometric-auxiliary", Required: []CapabilityRequirement{{Kind: capability, ProviderID: "face"}}}
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
		ScenarioRequirements{ID: "anonymous-fallback", Optional: []OptionalCapability{{
			Kind: FaceIdentification, ProviderID: "face", Fallback: AnonymousSubject,
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
				ScenarioRequirements{ID: "independent", Required: []CapabilityRequirement{{Kind: test.required, ProviderID: test.providerID}}},
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
			ScenarioRequirements{ID: "catalog", Required: []CapabilityRequirement{{Kind: capability, ProviderID: "provider"}}},
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
