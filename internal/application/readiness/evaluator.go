package readiness

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"proactive-interaction-engine/internal/domain/fault"
)

const evaluateOp = "evaluate scenario readiness"

// EvaluateAt validates the complete selection before deterministically
// evaluating provider availability and biometric policy at now.
func EvaluateAt(scenario ScenarioRequirements, providers []ProviderSnapshot, policy BiometricPolicySnapshot, now time.Time) (Activation, error) {
	if err := ValidateScenario(scenario); err != nil {
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
		if issue, found := evaluateCapability(requirement.Kind, requirement.ProviderID, requirement.Compatibility, "", providerByID, policy.Enabled, authorized, enrolled, now); found {
			issues = append(issues, issue)
			requiredBlocked = true
		}
	}
	for _, optional := range scenario.Optional {
		if issue, found := evaluateCapability(optional.Kind, optional.ProviderID, optional.Compatibility, optional.Fallback, providerByID, policy.Enabled, authorized, enrolled, now); found {
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

// ValidateScenario validates only the declarative capability selection. It
// does not require provider, biometric policy, or time inputs.
func ValidateScenario(scenario ScenarioRequirements) error {
	if scenario.ID == "" {
		return invalidInput("scenario id is required")
	}
	if len(scenario.Required)+len(scenario.Optional) == 0 {
		return invalidInput("scenario %q declares no capabilities", scenario.ID)
	}
	if !validIdentityAssurance(scenario.MinimumIdentityAssurance) {
		return invalidInput("scenario %q has unknown minimum identity assurance %q", scenario.ID, scenario.MinimumIdentityAssurance)
	}

	selected := make(map[CapabilityKind]struct{}, len(scenario.Required)+len(scenario.Optional))
	for _, requirement := range scenario.Required {
		if err := validateSelection(requirement.Kind, requirement.ProviderID, requirement.Compatibility, selected); err != nil {
			return err
		}
	}
	for _, optional := range scenario.Optional {
		if err := validateSelection(optional.Kind, optional.ProviderID, optional.Compatibility, selected); err != nil {
			return err
		}
		if !validFallback(optional.Kind, optional.Fallback) {
			return invalidInput("fallback %q is not valid for capability %q", optional.Fallback, optional.Kind)
		}
	}
	if err := validateIdentityAssurance(scenario.MinimumIdentityAssurance, selected); err != nil {
		return err
	}
	return nil
}

func validateSelection(kind CapabilityKind, providerID string, compatibility ProviderCompatibility, selected map[CapabilityKind]struct{}) error {
	if !validCapability(kind) {
		return invalidInput("unknown capability %q", kind)
	}
	if providerID == "" {
		return invalidInput("provider id is required for capability %q", kind)
	}
	if err := validateCompatibility(kind, compatibility); err != nil {
		return err
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
		if err := validateOperationalProfile(provider.ProviderID, provider.OperationalProfile); err != nil {
			return nil, err
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
	compatibility ProviderCompatibility,
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
		issue.Reason = IssueProviderMissing
		return issue, true
	}
	if !providerSupports(provider, kind) {
		issue.Code = fault.CapabilityMissing
		issue.Reason = IssueCapabilityMissing
		return issue, true
	}
	if provider.Health == Unhealthy {
		issue.Code = fault.Unavailable
		issue.Reason = IssueProviderUnhealthy
		return issue, true
	}
	if !now.Before(provider.LeaseExpiresAt) {
		issue.Code = fault.DeadlineExceeded
		issue.Reason = IssueLeaseExpired
		return issue, true
	}
	if code, reason := compatibilityIssue(compatibility, provider); reason != "" {
		issue.Code = code
		issue.Reason = reason
		return issue, true
	}
	if !isBiometric(kind) {
		return Issue{}, false
	}
	if !biometricEnabled {
		issue.Code = fault.PolicyBlocked
		issue.Reason = IssueBiometricDisabled
		return issue, true
	}
	if _, exists := authorized[kind]; !exists {
		issue.Code = fault.PermissionDenied
		issue.Reason = IssueBiometricUnauthorized
		return issue, true
	}
	if requiresEnrollment(kind) {
		if _, exists := enrolled[kind]; !exists {
			issue.Code = fault.PolicyBlocked
			issue.Reason = IssueEnrollmentMissing
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
	case FaceDetection, FaceIdentification, FaceLiveness, SpeakerIdentification, SpeakerVerification:
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

func validIdentityAssurance(assurance IdentityAssurance) bool {
	switch assurance {
	case IdentityAssuranceAnonymous, IdentityAssuranceRecognized, IdentityAssuranceVerified:
		return true
	default:
		return false
	}
}

func validateIdentityAssurance(assurance IdentityAssurance, selected map[CapabilityKind]struct{}) error {
	switch assurance {
	case IdentityAssuranceAnonymous:
		return nil
	case IdentityAssuranceRecognized:
		for _, capability := range []CapabilityKind{FaceIdentification, SpeakerIdentification, SpeakerVerification} {
			if _, exists := selected[capability]; exists {
				return nil
			}
		}
		return invalidInput("minimum identity assurance %q requires an identification or verification capability", assurance)
	case IdentityAssuranceVerified:
		if _, exists := selected[SpeakerVerification]; !exists {
			return invalidInput("minimum identity assurance %q requires capability %q", assurance, SpeakerVerification)
		}
		return nil
	default:
		return invalidInput("unknown minimum identity assurance %q", assurance)
	}
}

func validateCompatibility(kind CapabilityKind, compatibility ProviderCompatibility) error {
	if !validRequiredString(compatibility.ProtocolVersion) {
		return invalidInput("exact protocol version is required for capability %q", kind)
	}
	if compatibility.MaximumLatency <= 0 {
		return invalidInput("maximum latency must be positive for capability %q", kind)
	}
	if err := validateUniquePrivacyClasses("allowed", compatibility.AllowedPrivacyClasses, true); err != nil {
		return invalidInput("capability %q compatibility: %v", kind, err)
	}
	if err := validateUniqueCancellationSemantics("allowed", compatibility.AllowedCancellationSemantics, true); err != nil {
		return invalidInput("capability %q compatibility: %v", kind, err)
	}
	if err := validateUniqueDeviceClasses("allowed", compatibility.AllowedDeviceClasses); err != nil {
		return invalidInput("capability %q compatibility: %v", kind, err)
	}
	return nil
}

func validateOperationalProfile(providerID string, profile ProviderOperationalProfile) error {
	if !validPrivacyClass(profile.PrivacyClass) {
		return invalidInput("provider %q has unknown privacy class %q", providerID, profile.PrivacyClass)
	}
	if profile.MaximumLatency <= 0 {
		return invalidInput("provider %q maximum latency must be positive", providerID)
	}
	if !validCancellationSemantics(profile.CancellationSemantics) {
		return invalidInput("provider %q has unknown cancellation semantics %q", providerID, profile.CancellationSemantics)
	}
	if err := validateUniqueDeviceClasses("provider", profile.DeviceRequirements); err != nil {
		return invalidInput("provider %q operational profile: %v", providerID, err)
	}
	return nil
}

func compatibilityIssue(compatibility ProviderCompatibility, provider ProviderSnapshot) (fault.Code, IssueReason) {
	if provider.ProtocolVersion != compatibility.ProtocolVersion {
		return fault.CapabilityMissing, IssueProtocolIncompatible
	}
	if !containsPrivacyClass(compatibility.AllowedPrivacyClasses, provider.OperationalProfile.PrivacyClass) {
		return fault.PolicyBlocked, IssuePrivacyIncompatible
	}
	if provider.OperationalProfile.MaximumLatency > compatibility.MaximumLatency {
		return fault.DeadlineExceeded, IssueLatencyIncompatible
	}
	if !containsCancellationSemantics(compatibility.AllowedCancellationSemantics, provider.OperationalProfile.CancellationSemantics) {
		return fault.CapabilityMissing, IssueCancellationIncompatible
	}
	allowedDevices := make(map[ProviderDeviceClass]struct{}, len(compatibility.AllowedDeviceClasses))
	for _, device := range compatibility.AllowedDeviceClasses {
		allowedDevices[device] = struct{}{}
	}
	for _, required := range provider.OperationalProfile.DeviceRequirements {
		if _, allowed := allowedDevices[required]; !allowed {
			return fault.CapabilityMissing, IssueDeviceIncompatible
		}
	}
	return "", ""
}

func validateUniquePrivacyClasses(label string, values []ProviderPrivacyClass, requireNonEmpty bool) error {
	if requireNonEmpty && len(values) == 0 {
		return fmt.Errorf("%s privacy classes must not be empty", label)
	}
	seen := make(map[ProviderPrivacyClass]struct{}, len(values))
	for _, value := range values {
		if !validPrivacyClass(value) {
			return fmt.Errorf("%s privacy class %q is unknown", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s privacy class %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUniqueCancellationSemantics(label string, values []ProviderCancellationSemantics, requireNonEmpty bool) error {
	if requireNonEmpty && len(values) == 0 {
		return fmt.Errorf("%s cancellation semantics must not be empty", label)
	}
	seen := make(map[ProviderCancellationSemantics]struct{}, len(values))
	for _, value := range values {
		if !validCancellationSemantics(value) {
			return fmt.Errorf("%s cancellation semantics %q is unknown", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s cancellation semantics %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUniqueDeviceClasses(label string, values []ProviderDeviceClass) error {
	seen := make(map[ProviderDeviceClass]struct{}, len(values))
	for _, value := range values {
		if !validDeviceClass(value) {
			return fmt.Errorf("%s device class %q is unknown", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s device class %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func containsPrivacyClass(values []ProviderPrivacyClass, expected ProviderPrivacyClass) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsCancellationSemantics(values []ProviderCancellationSemantics, expected ProviderCancellationSemantics) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validPrivacyClass(value ProviderPrivacyClass) bool {
	return value == ProviderPrivacyDeviceLocal || value == ProviderPrivacyRemoteProcessing
}

func validCancellationSemantics(value ProviderCancellationSemantics) bool {
	switch value {
	case ProviderCancellationNotSupported, ProviderCancellationCooperative, ProviderCancellationBounded:
		return true
	default:
		return false
	}
}

func validDeviceClass(value ProviderDeviceClass) bool {
	switch value {
	case ProviderDeviceCamera, ProviderDeviceMicrophone, ProviderDeviceDisplay,
		ProviderDeviceAudioOutput, ProviderDeviceEmbodimentController:
		return true
	default:
		return false
	}
}

func validRequiredString(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
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
