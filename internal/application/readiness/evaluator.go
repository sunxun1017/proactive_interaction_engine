package readiness

import (
	"fmt"
	"sort"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

const evaluateOp = "evaluate scenario readiness"

// EvaluateAt validates the complete selection before deterministically
// evaluating provider availability and biometric policy at now.
func EvaluateAt(scenario ScenarioRequirements, providers []ProviderSnapshot, policy BiometricPolicySnapshot, now time.Time) (Activation, error) {
	if err := validateScenario(scenario); err != nil {
		return Activation{}, err
	}
	providerByID, err := validateProviders(providers)
	if err != nil {
		return Activation{}, err
	}
	authorized, enrolled, err := validateBiometricPolicy(policy)
	if err != nil {
		return Activation{}, err
	}

	issues := make([]Issue, 0, len(scenario.Required)+len(scenario.Optional))
	requiredBlocked := false
	for _, requirement := range scenario.Required {
		if issue, found := evaluateCapability(requirement.Kind, requirement.ProviderID, "", providerByID, policy.Enabled, authorized, enrolled, now); found {
			issues = append(issues, issue)
			requiredBlocked = true
		}
	}
	for _, optional := range scenario.Optional {
		if issue, found := evaluateCapability(optional.Kind, optional.ProviderID, optional.Fallback, providerByID, policy.Enabled, authorized, enrolled, now); found {
			issues = append(issues, issue)
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Capability != issues[j].Capability {
			return issues[i].Capability < issues[j].Capability
		}
		return issues[i].ProviderID < issues[j].ProviderID
	})

	status := Ready
	if requiredBlocked {
		status = Blocked
	} else if len(issues) > 0 {
		status = Degraded
	}
	return Activation{Status: status, Issues: issues}, nil
}

func validateScenario(scenario ScenarioRequirements) error {
	if scenario.ID == "" {
		return invalidInput("scenario id is required")
	}
	if len(scenario.Required)+len(scenario.Optional) == 0 {
		return invalidInput("scenario %q declares no capabilities", scenario.ID)
	}

	selected := make(map[CapabilityKind]struct{}, len(scenario.Required)+len(scenario.Optional))
	for _, requirement := range scenario.Required {
		if err := validateSelection(requirement.Kind, requirement.ProviderID, selected); err != nil {
			return err
		}
	}
	for _, optional := range scenario.Optional {
		if err := validateSelection(optional.Kind, optional.ProviderID, selected); err != nil {
			return err
		}
		if !validFallback(optional.Kind, optional.Fallback) {
			return invalidInput("fallback %q is not valid for capability %q", optional.Fallback, optional.Kind)
		}
	}
	return nil
}

func validateSelection(kind CapabilityKind, providerID string, selected map[CapabilityKind]struct{}) error {
	if !validCapability(kind) {
		return invalidInput("unknown capability %q", kind)
	}
	if providerID == "" {
		return invalidInput("provider id is required for capability %q", kind)
	}
	if _, exists := selected[kind]; exists {
		return invalidInput("capability %q is selected more than once", kind)
	}
	selected[kind] = struct{}{}
	return nil
}

func validateProviders(providers []ProviderSnapshot) (map[string]ProviderSnapshot, error) {
	providerByID := make(map[string]ProviderSnapshot, len(providers))
	for _, provider := range providers {
		if provider.ProviderID == "" || provider.InstanceID == "" || provider.ProtocolVersion == "" || provider.ImplementationVersion == "" {
			return nil, invalidInput("provider id, instance id, protocol version, and implementation version are required")
		}
		if provider.LeaseExpiresAt.IsZero() {
			return nil, invalidInput("provider %q lease expiry is required", provider.ProviderID)
		}
		if provider.Health != Healthy && provider.Health != Unhealthy {
			return nil, invalidInput("provider %q has unknown health %q", provider.ProviderID, provider.Health)
		}
		if len(provider.Capabilities) == 0 {
			return nil, invalidInput("provider %q declares no capabilities", provider.ProviderID)
		}
		seenCapabilities := make(map[CapabilityKind]struct{}, len(provider.Capabilities))
		for _, capability := range provider.Capabilities {
			if !validCapability(capability) {
				return nil, invalidInput("provider %q declares unknown capability %q", provider.ProviderID, capability)
			}
			if _, exists := seenCapabilities[capability]; exists {
				return nil, invalidInput("provider %q declares capability %q more than once", provider.ProviderID, capability)
			}
			seenCapabilities[capability] = struct{}{}
		}
		if _, exists := providerByID[provider.ProviderID]; exists {
			return nil, invalidInput("provider %q has more than one snapshot", provider.ProviderID)
		}
		providerByID[provider.ProviderID] = provider
	}
	return providerByID, nil
}

func validateBiometricPolicy(policy BiometricPolicySnapshot) (map[CapabilityKind]struct{}, map[CapabilityKind]struct{}, error) {
	authorized, err := validatePolicyKinds("authorized", policy.Authorized, isBiometric)
	if err != nil {
		return nil, nil, err
	}
	enrolled, err := validatePolicyKinds("enrolled", policy.Enrolled, requiresEnrollment)
	if err != nil {
		return nil, nil, err
	}
	return authorized, enrolled, nil
}

func validatePolicyKinds(label string, capabilities []CapabilityKind, allowed func(CapabilityKind) bool) (map[CapabilityKind]struct{}, error) {
	validated := make(map[CapabilityKind]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !validCapability(capability) || !allowed(capability) {
			return nil, invalidInput("%s capability %q is not allowed", label, capability)
		}
		if _, exists := validated[capability]; exists {
			return nil, invalidInput("%s capability %q is duplicated", label, capability)
		}
		validated[capability] = struct{}{}
	}
	return validated, nil
}

func evaluateCapability(
	kind CapabilityKind,
	providerID string,
	fallback Fallback,
	providers map[string]ProviderSnapshot,
	biometricEnabled bool,
	authorized map[CapabilityKind]struct{},
	enrolled map[CapabilityKind]struct{},
	now time.Time,
) (Issue, bool) {
	issue := Issue{Capability: kind, ProviderID: providerID, Fallback: fallback}
	provider, exists := providers[providerID]
	if !exists {
		issue.Code = fault.Unavailable
		return issue, true
	}
	if !providerSupports(provider, kind) {
		issue.Code = fault.CapabilityMissing
		return issue, true
	}
	if provider.Health == Unhealthy {
		issue.Code = fault.Unavailable
		return issue, true
	}
	if !now.Before(provider.LeaseExpiresAt) {
		issue.Code = fault.DeadlineExceeded
		return issue, true
	}
	if !isBiometric(kind) {
		return Issue{}, false
	}
	if !biometricEnabled {
		issue.Code = fault.PolicyBlocked
		return issue, true
	}
	if _, exists := authorized[kind]; !exists {
		issue.Code = fault.PermissionDenied
		return issue, true
	}
	if requiresEnrollment(kind) {
		if _, exists := enrolled[kind]; !exists {
			issue.Code = fault.PolicyBlocked
			return issue, true
		}
	}
	return Issue{}, false
}

func providerSupports(provider ProviderSnapshot, required CapabilityKind) bool {
	for _, capability := range provider.Capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

func validCapability(kind CapabilityKind) bool {
	switch kind {
	case PersonPresence, FaceDetection, FaceIdentification, FaceLiveness,
		VoiceActivity, SpeakerIdentification, SpeakerVerification,
		SpeechTranscription, AttentionEstimation, BusyState, GestureDetection,
		DeviceState, DisplayText, AvatarAttend, AvatarExpression,
		SpeechSynthesis, Gesture, Light, Locomotion:
		return true
	default:
		return false
	}
}

func validFallback(kind CapabilityKind, fallback Fallback) bool {
	switch kind {
	case FaceIdentification, FaceLiveness, SpeakerIdentification, SpeakerVerification:
		return fallback == AnonymousSubject
	case VoiceActivity:
		return fallback == NoVoiceReply
	case SpeechTranscription:
		return fallback == NoTranscript
	case SpeechSynthesis:
		return fallback == VisualOnly
	case DisplayText, AvatarAttend, AvatarExpression:
		return fallback == AudioOnly
	default:
		return false
	}
}

func isBiometric(kind CapabilityKind) bool {
	switch kind {
	case FaceDetection, FaceIdentification, FaceLiveness, SpeakerIdentification, SpeakerVerification:
		return true
	default:
		return false
	}
}

func requiresEnrollment(kind CapabilityKind) bool {
	switch kind {
	case FaceIdentification, SpeakerIdentification, SpeakerVerification:
		return true
	default:
		return false
	}
}

func invalidInput(format string, args ...any) error {
	return fault.New(fault.InvalidInput, evaluateOp, fmt.Errorf(format, args...))
}
