package readiness

// ProviderMatchesCompatibility reports whether one validated provider falls
// within the exact operational envelope selected for a capability. It does
// not evaluate health, lease expiry, capability declaration, or policy.
func ProviderMatchesCompatibility(kind CapabilityKind, compatibility ProviderCompatibility, provider ProviderSnapshot) (bool, error) {
	if err := validateCompatibility(kind, compatibility); err != nil {
		return false, err
	}
	providers, err := validateProviders([]ProviderSnapshot{provider})
	if err != nil {
		return false, err
	}
	validated := providers[provider.ProviderID]
	_, reason := compatibilityIssue(compatibility, validated)
	return reason == "", nil
}
