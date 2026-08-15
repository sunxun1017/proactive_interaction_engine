package main

import (
	"context"
	"errors"
	"fmt"

	identityingress "proactive-interaction-engine/adapters/identity/ingress"
	"proactive-interaction-engine/adapters/storage/biometricvault"
	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
)

type identityMasterKeyProviderFactory func(
	biometricvault.SecretServiceMasterKeyConfig,
) (biometricvault.MasterKeyProvider, error)

// desktopIdentityComposition retains the complete synchronous identity graph.
// It owns no goroutine or long-lived Secret Service session.
type desktopIdentityComposition struct {
	catalog   *biometric.Service
	vault     *biometricvault.Vault
	lifecycle *biometricvault.TemplateLifecycleCoordinator
	runtime   *desktopIdentityRuntime
	ingress   *identityingress.Server
	readiness *activationIdentityReadiness
}

func newDesktopIdentityComposition(
	ctx context.Context,
	config IdentityConfig,
	permissions identityPermissionReader,
	leases identityingress.ProviderLeaseReader,
	clock port.Clock,
	scenario readiness.ScenarioRequirements,
	newMasterKeyProvider identityMasterKeyProviderFactory,
) (*desktopIdentityComposition, error) {
	const op = "compose desktop identity"
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return nil, classifyDesktopIdentityFailure(op, err)
	}
	if err := config.validate(); err != nil || !config.Enabled {
		if err == nil {
			err = errors.New("identity must be enabled")
		}
		return nil, fault.New(fault.InvalidInput, op, err)
	}
	if isNilDesktopIdentityDependency(permissions) || isNilDesktopIdentityDependency(leases) || isNilDesktopIdentityDependency(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("permissions, provider leases, and clock are required"))
	}
	if newMasterKeyProvider == nil {
		return nil, fault.New(fault.InvalidInput, op, errors.New("master-key provider factory is required"))
	}
	if err := readiness.ValidateScenario(scenario); err != nil {
		return nil, fmt.Errorf("%s: validate scenario: %w", op, err)
	}

	keys, err := newMasterKeyProvider(biometricvault.SecretServiceMasterKeyConfig{
		VaultRoot: config.VaultRoot, RuntimeDirectory: config.SecretServiceRuntimeDirectory,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: create master-key provider: %w", op, err)
	}
	repository, err := biometricvault.NewCatalogRepository(config.VaultRoot, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: create catalog repository: %w", op, err)
	}
	catalog, err := biometric.New(ctx, repository, clock)
	if err != nil {
		return nil, fmt.Errorf("%s: load biometric catalog: %w", op, err)
	}
	vault, err := biometricvault.New(config.VaultRoot, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: create biometric vault: %w", op, err)
	}
	lifecycle, err := biometricvault.NewTemplateLifecycleCoordinator(catalog, vault)
	if err != nil {
		return nil, fmt.Errorf("%s: create template lifecycle coordinator: %w", op, err)
	}
	if _, err := lifecycle.Recover(ctx); err != nil {
		return nil, fmt.Errorf("%s: recover biometric template lifecycle: %w", op, err)
	}

	runtime, err := newDesktopIdentityRuntime(desktopIdentityRuntimeConfig{
		Policy: config.Policy, Identification: config.Identification, Verification: config.Verification,
	}, permissions, catalog, clock)
	if err != nil {
		return nil, fmt.Errorf("%s: create runtime: %w", op, err)
	}
	ingress, err := identityingress.NewServer(
		identityingress.Config{MaxTrackedSources: config.MaxTrackedSources},
		leases, permissions, runtime.identification, runtime.verification, clock, scenario,
	)
	if err != nil {
		return nil, fmt.Errorf("%s: create evidence ingress: %w", op, err)
	}
	identityReadiness := &activationIdentityReadiness{
		BiometricProfileRef: config.BiometricProfileRef,
		Privacy:             permissions,
		Catalog:             catalog,
	}
	return &desktopIdentityComposition{
		catalog: catalog, vault: vault, lifecycle: lifecycle, runtime: runtime,
		ingress: ingress, readiness: identityReadiness,
	}, nil
}
