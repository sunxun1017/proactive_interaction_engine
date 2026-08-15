package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
)

type providerSnapshotReader interface {
	Snapshots() []readiness.ProviderSnapshot
}

type activationPrivacySnapshotReader interface {
	Current(context.Context) (privacy.Snapshot, error)
}

type activationBiometricPolicyReader interface {
	ReadinessPolicy(privacy.Snapshot, string) (readiness.BiometricPolicySnapshot, error)
}

type activationIdentityReadiness struct {
	BiometricProfileRef string
	Privacy             activationPrivacySnapshotReader
	Catalog             activationBiometricPolicyReader
}

type activationViewOptions struct {
	TTSEnabled bool
	Identity   *activationIdentityReadiness
}

type activationView struct {
	scenario   readiness.ScenarioRequirements
	providers  providerSnapshotReader
	clock      port.Clock
	ttsEnabled bool
	identity   *activationIdentityReadiness
}

func newActivationView(
	scenario readiness.ScenarioRequirements,
	providers providerSnapshotReader,
	clock port.Clock,
	options activationViewOptions,
) (*activationView, error) {
	if isNilActivationDependency(providers) || isNilActivationDependency(clock) {
		return nil, errors.New("provider snapshots and clock are required")
	}
	if err := readiness.ValidateScenario(scenario); err != nil {
		return nil, err
	}

	var identity *activationIdentityReadiness
	if options.Identity != nil {
		profileRef := options.Identity.BiometricProfileRef
		trimmedProfileRef := strings.TrimSpace(profileRef)
		if trimmedProfileRef == "" || trimmedProfileRef != profileRef {
			return nil, errors.New("biometric profile reference must be non-empty and have no surrounding whitespace")
		}
		if isNilActivationDependency(options.Identity.Privacy) || isNilActivationDependency(options.Identity.Catalog) {
			return nil, errors.New("privacy snapshots and biometric readiness catalog are required when identity readiness is enabled")
		}
		configured := *options.Identity
		identity = &configured
	}
	return &activationView{
		scenario: cloneScenario(scenario), providers: providers, clock: clock,
		ttsEnabled: options.TTSEnabled, identity: identity,
	}, nil
}

func (v *activationView) CurrentActivation() readiness.Activation {
	now := v.clock.Now()
	policy := readiness.BiometricPolicySnapshot{}
	if v.identity != nil {
		global, err := v.identity.Privacy.Current(context.Background())
		if err != nil {
			return readiness.Activation{Status: readiness.Blocked}
		}
		policy, err = v.identity.Catalog.ReadinessPolicy(global, v.identity.BiometricProfileRef)
		if err != nil {
			return readiness.Activation{Status: readiness.Blocked}
		}
	}
	providers := v.providers.Snapshots()
	providers = append(providers, localProvider("web-avatar", readiness.DisplayText, now))
	if v.ttsEnabled {
		providers = append(providers, localProvider("speech-dispatcher", readiness.SpeechSynthesis, now))
	}
	activation, err := readiness.EvaluateAt(v.scenario, providers, policy, now)
	if err != nil {
		return readiness.Activation{Status: readiness.Blocked}
	}
	return activation
}

func isNilActivationDependency(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}

func localProvider(id string, capability readiness.CapabilityKind, now time.Time) readiness.ProviderSnapshot {
	profile := readiness.ProviderOperationalProfile{
		PrivacyClass:          readiness.ProviderPrivacyDeviceLocal,
		CancellationSemantics: readiness.ProviderCancellationCooperative,
	}
	switch capability {
	case readiness.DisplayText:
		profile.MaximumLatency = 50 * time.Millisecond
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceDisplay}
	case readiness.SpeechSynthesis:
		profile.MaximumLatency = time.Second
		profile.DeviceRequirements = []readiness.ProviderDeviceClass{readiness.ProviderDeviceAudioOutput}
	}
	return readiness.ProviderSnapshot{
		ProviderID: id, InstanceID: id + "-in-process", ProtocolVersion: "v1", ImplementationVersion: "desktop.v1",
		Capabilities: []readiness.CapabilityKind{capability}, Health: readiness.Healthy, LeaseExpiresAt: now.Add(time.Minute),
		OperationalProfile: profile,
	}
}

func cloneScenario(input readiness.ScenarioRequirements) readiness.ScenarioRequirements {
	input.Required = append([]readiness.CapabilityRequirement(nil), input.Required...)
	for index := range input.Required {
		input.Required[index].Compatibility = cloneCompatibility(input.Required[index].Compatibility)
	}
	input.Optional = append([]readiness.OptionalCapability(nil), input.Optional...)
	for index := range input.Optional {
		input.Optional[index].Compatibility = cloneCompatibility(input.Optional[index].Compatibility)
	}
	return input
}

func cloneCompatibility(input readiness.ProviderCompatibility) readiness.ProviderCompatibility {
	input.AllowedPrivacyClasses = append([]readiness.ProviderPrivacyClass(nil), input.AllowedPrivacyClasses...)
	input.AllowedCancellationSemantics = append([]readiness.ProviderCancellationSemantics(nil), input.AllowedCancellationSemantics...)
	input.AllowedDeviceClasses = append([]readiness.ProviderDeviceClass(nil), input.AllowedDeviceClasses...)
	return input
}

func cloneProviderSnapshots(input []readiness.ProviderSnapshot) []readiness.ProviderSnapshot {
	output := append([]readiness.ProviderSnapshot(nil), input...)
	for index := range output {
		output[index].Capabilities = append([]readiness.CapabilityKind(nil), output[index].Capabilities...)
		output[index].OperationalProfile.DeviceRequirements = append(
			[]readiness.ProviderDeviceClass(nil), output[index].OperationalProfile.DeviceRequirements...,
		)
	}
	return output
}
