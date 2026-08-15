package main

import (
	"errors"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/readiness"
)

type providerSnapshotReader interface {
	Snapshots() []readiness.ProviderSnapshot
}

type activationView struct {
	scenario   readiness.ScenarioRequirements
	providers  providerSnapshotReader
	clock      port.Clock
	ttsEnabled bool
}

func newActivationView(scenario readiness.ScenarioRequirements, providers providerSnapshotReader, clock port.Clock, ttsEnabled bool) (*activationView, error) {
	if providers == nil || clock == nil {
		return nil, errors.New("provider snapshots and clock are required")
	}
	if err := readiness.ValidateScenario(scenario); err != nil {
		return nil, err
	}
	return &activationView{scenario: cloneScenario(scenario), providers: providers, clock: clock, ttsEnabled: ttsEnabled}, nil
}

func (v *activationView) CurrentActivation() readiness.Activation {
	now := v.clock.Now()
	providers := v.providers.Snapshots()
	providers = append(providers, localProvider("web-avatar", readiness.DisplayText, now))
	if v.ttsEnabled {
		providers = append(providers, localProvider("speech-dispatcher", readiness.SpeechSynthesis, now))
	}
	activation, err := readiness.EvaluateAt(v.scenario, providers, readiness.BiometricPolicySnapshot{}, now)
	if err != nil {
		return readiness.Activation{Status: readiness.Blocked}
	}
	return activation
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
