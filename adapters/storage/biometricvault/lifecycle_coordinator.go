package biometricvault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/domain/fault"
)

const templateLifecycleOp = "coordinate biometric template lifecycle"

type prepareStoreFunc func(context.Context, biometric.Registration, string) (biometric.Snapshot, error)

// TemplateLifecycleCoordinator is the single process-local owner of template
// registration, replacement, recovery, and deletion for one catalog/vault
// pair. Filesystem locks do not extend this ownership into a distributed
// transaction; composition must construct only one owner per vault root.
type TemplateLifecycleCoordinator struct {
	mu       sync.Mutex
	catalog  *biometric.Service
	vault    *Vault
	deletion *biometric.DeletionCoordinator
	random   io.Reader
}

// NewTemplateLifecycleCoordinator constructs the dedicated encrypted-template
// boundary. It performs no I/O; callers explicitly invoke Recover at startup.
func NewTemplateLifecycleCoordinator(catalog *biometric.Service, vault *Vault) (*TemplateLifecycleCoordinator, error) {
	return newTemplateLifecycleCoordinator(catalog, vault, rand.Reader)
}

func newTemplateLifecycleCoordinator(
	catalog *biometric.Service,
	vault *Vault,
	random io.Reader,
) (*TemplateLifecycleCoordinator, error) {
	if isNil(catalog) || isNil(vault) || isNil(random) {
		return nil, fault.New(fault.InvalidInput, templateLifecycleOp, errors.New("catalog, vault, and entropy source are required"))
	}
	deletion, err := biometric.NewDeletionCoordinator(catalog, vault)
	if err != nil {
		return nil, fmt.Errorf("%s: construct deletion coordinator: %w", templateLifecycleOp, err)
	}
	return &TemplateLifecycleCoordinator{catalog: catalog, vault: vault, deletion: deletion, random: random}, nil
}

// Register validates one fixed-codec template, durably prepares its opaque
// metadata, stores encrypted bytes, and only then activates the reference.
func (c *TemplateLifecycleCoordinator) Register(
	ctx context.Context,
	registration biometric.Registration,
	encodedTemplate []byte,
) (biometric.Snapshot, error) {
	return c.storeAndCommit(ctx, registration, encodedTemplate, c.catalog.PrepareRegister, false)
}

// Replace keeps the old enrollment active until the replacement is durably
// stored and committed, then retries deletion of the retired reference.
func (c *TemplateLifecycleCoordinator) Replace(
	ctx context.Context,
	registration biometric.Registration,
	encodedTemplate []byte,
) (biometric.Snapshot, error) {
	return c.storeAndCommit(ctx, registration, encodedTemplate, c.catalog.PrepareReplace, true)
}

func (c *TemplateLifecycleCoordinator) storeAndCommit(
	ctx context.Context,
	registration biometric.Registration,
	encodedTemplate []byte,
	prepare prepareStoreFunc,
	cleanupRetired bool,
) (biometric.Snapshot, error) {
	if err := validateContext(templateLifecycleOp, ctx); err != nil {
		return biometric.Snapshot{}, err
	}
	if err := validateTemplatePayload(registration, encodedTemplate); err != nil {
		return biometric.Snapshot{}, err
	}
	templateCopy := append([]byte(nil), encodedTemplate...)
	defer clear(templateCopy)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalog.ReloadRequired() {
		return biometric.Snapshot{}, fault.New(fault.Unavailable, templateLifecycleOp, errors.New("catalog reload is required"))
	}

	current := c.catalog.Current()
	if record, found := enrollmentRecord(current, registration); found && record.PendingStore == nil &&
		record.Status == biometric.EnrollmentActive &&
		record.TemplateRef == registration.TemplateRef && record.ModelVersion == registration.ModelVersion {
		if err := c.verifyCommittedTemplate(ctx, registration, templateCopy); err != nil {
			return biometric.Snapshot{}, err
		}
		return c.cleanupRetired(ctx, current, registration, cleanupRetired)
	}

	storeOperationID := ""
	if record, found := enrollmentRecord(current, registration); found && pendingMatches(record.PendingStore, registration) {
		storeOperationID = record.PendingStore.StoreOperationID
	} else {
		var err error
		storeOperationID, err = c.newStoreOperationID()
		if err != nil {
			return biometric.Snapshot{}, err
		}
	}

	prepared, err := prepare(ctx, registration, storeOperationID)
	if err != nil {
		return biometric.Snapshot{}, fmt.Errorf("%s: prepare template metadata: %w", templateLifecycleOp, err)
	}
	if record, found := enrollmentRecord(prepared, registration); found && record.PendingStore == nil &&
		record.Status == biometric.EnrollmentActive &&
		record.TemplateRef == registration.TemplateRef && record.ModelVersion == registration.ModelVersion {
		if err := c.verifyCommittedTemplate(ctx, registration, templateCopy); err != nil {
			return biometric.Snapshot{}, err
		}
		return c.cleanupRetired(ctx, prepared, registration, cleanupRetired)
	}
	if err := c.vault.store(ctx, descriptorForRegistration(registration), storeOperationID, templateCopy); err != nil {
		return prepared, fmt.Errorf("%s: store encrypted template: %w", templateLifecycleOp, err)
	}
	committed, err := c.catalog.CommitPrepared(ctx, registration, storeOperationID)
	if err != nil {
		// The repository may have renamed the committed catalog before reporting
		// an error. Never delete the stored template from this stale view.
		return biometric.Snapshot{}, fmt.Errorf("%s: commit template metadata: %w", templateLifecycleOp, err)
	}
	return c.cleanupRetired(ctx, committed, registration, cleanupRetired)
}

func (c *TemplateLifecycleCoordinator) cleanupRetired(
	ctx context.Context,
	snapshot biometric.Snapshot,
	registration biometric.Registration,
	required bool,
) (biometric.Snapshot, error) {
	if !required {
		return snapshot, nil
	}
	cleaned, err := c.deletion.RetryPendingForKey(ctx, biometric.EnrollmentKey{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
	})
	if err != nil {
		return cleaned, fmt.Errorf("%s: delete retired template: %w", templateLifecycleOp, err)
	}
	return cleaned, nil
}

func (c *TemplateLifecycleCoordinator) verifyCommittedTemplate(
	ctx context.Context,
	registration biometric.Registration,
	expected []byte,
) error {
	payload, _, found, err := c.vault.loadIfPresent(ctx, descriptorForRegistration(registration), "verify committed biometric template")
	if err != nil {
		return fmt.Errorf("%s: verify committed template: %w", templateLifecycleOp, err)
	}
	if !found {
		return fault.New(fault.AdapterRejected, templateLifecycleOp, errors.New("active template is absent from the vault"))
	}
	defer clear(payload)
	if !bytes.Equal(payload, expected) {
		return fault.New(fault.StaleInput, templateLifecycleOp, errors.New("active template payload does not match"))
	}
	return nil
}

func (c *TemplateLifecycleCoordinator) newStoreOperationID() (string, error) {
	var operation [16]byte
	if _, err := io.ReadFull(c.random, operation[:]); err != nil {
		return "", fault.New(fault.Unavailable, templateLifecycleOp, fmt.Errorf("generate store operation ID: %w", err))
	}
	return hex.EncodeToString(operation[:]), nil
}

// Recover reloads the durable catalog before inspecting any template. It
// resolves staged stores in canonical order, then retries physical deletions.
func (c *TemplateLifecycleCoordinator) Recover(ctx context.Context) (biometric.Snapshot, error) {
	if err := validateContext(templateLifecycleOp, ctx); err != nil {
		return biometric.Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, err := c.catalog.Reload(ctx)
	if err != nil {
		return biometric.Snapshot{}, fmt.Errorf("%s: reload catalog: %w", templateLifecycleOp, err)
	}
	var firstErr error
	remember := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, record := range snapshot.Records {
		if record.PendingStore == nil {
			continue
		}
		recoveryErr, stop := c.recoverPrepared(ctx, record)
		remember(recoveryErr)
		if stop {
			return biometric.Snapshot{}, firstErr
		}
	}
	cleaned, err := c.deletion.RetryPendingDeletes(ctx)
	remember(wrapLifecycleError("retry pending deletion", err))
	if c.catalog.ReloadRequired() {
		return biometric.Snapshot{}, firstErr
	}
	return cleaned, firstErr
}

func (c *TemplateLifecycleCoordinator) recoverPrepared(ctx context.Context, record biometric.Record) (error, bool) {
	registration := pendingRegistration(record)
	storeOperationID := record.PendingStore.StoreOperationID
	if !preparedStoreAuthorized(record) {
		_, err := c.deletePrepared(ctx, registration, storeOperationID)
		return wrapLifecycleError("discard unauthorized prepared template", err), c.catalog.ReloadRequired()
	}

	payload, actualOperationID, found, err := c.vault.loadIfPresent(
		ctx, descriptorForRegistration(registration), "recover biometric template",
	)
	if err != nil {
		return fmt.Errorf("%s: inspect prepared template: %w", templateLifecycleOp, err), false
	}
	if !found {
		_, err := c.catalog.AbortPrepared(ctx, registration, storeOperationID)
		return wrapLifecycleError("abort absent prepared template", err), c.catalog.ReloadRequired()
	}
	defer clear(payload)

	if actualOperationID != storeOperationID {
		if _, err := c.deletePrepared(ctx, registration, storeOperationID); err != nil {
			return err, c.catalog.ReloadRequired()
		}
		return fault.New(
			fault.StaleInput, templateLifecycleOp,
			errors.New("stored template belongs to a different prepared operation"),
		), false
	}
	if codecErr := validateTemplatePayload(registration, payload); codecErr != nil {
		if _, err := c.deletePrepared(ctx, registration, storeOperationID); err != nil {
			return err, c.catalog.ReloadRequired()
		}
		return fault.New(
			fault.AdapterRejected, templateLifecycleOp,
			fmt.Errorf("stored template violates its fixed codec: %w", codecErr),
		), false
	}
	if _, err := c.catalog.CommitPrepared(ctx, registration, storeOperationID); err != nil {
		// Commit may already be durable. Reload is required before any cleanup.
		return fmt.Errorf("%s: commit recovered template: %w", templateLifecycleOp, err), c.catalog.ReloadRequired()
	}
	return nil, false
}

// Delete cancels an uncommitted store for the enrollment before delegating the
// existing fail-closed current/retired deletion protocol.
func (c *TemplateLifecycleCoordinator) Delete(ctx context.Context, key biometric.EnrollmentKey) (biometric.Snapshot, error) {
	if err := validateContext(templateLifecycleOp, ctx); err != nil {
		return biometric.Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.deletePreparedForKey(ctx, key); err != nil {
		return biometric.Snapshot{}, err
	}
	return c.deletion.Delete(ctx, key)
}

// DeleteProfile cancels staged stores, revokes every profile consent, and
// attempts all current/retired physical deletions in canonical order.
func (c *TemplateLifecycleCoordinator) DeleteProfile(ctx context.Context, profileRef string) (biometric.Snapshot, error) {
	if err := validateContext(templateLifecycleOp, ctx); err != nil {
		return biometric.Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for _, record := range c.catalog.Current().Records {
		if record.ProfileRef != profileRef || record.PendingStore == nil {
			continue
		}
		if _, err := c.deletePrepared(ctx, pendingRegistration(record), record.PendingStore.StoreOperationID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if c.catalog.ReloadRequired() {
				return biometric.Snapshot{}, firstErr
			}
		}
	}
	snapshot, err := c.deletion.DeleteProfile(ctx, profileRef)
	if firstErr != nil {
		return snapshot, firstErr
	}
	return snapshot, err
}

func (c *TemplateLifecycleCoordinator) deletePreparedForKey(ctx context.Context, key biometric.EnrollmentKey) error {
	for _, record := range c.catalog.Current().Records {
		if record.ProfileRef != key.ProfileRef || record.Capability != key.Capability || record.PendingStore == nil {
			continue
		}
		_, err := c.deletePrepared(ctx, pendingRegistration(record), record.PendingStore.StoreOperationID)
		return err
	}
	return nil
}

func (c *TemplateLifecycleCoordinator) deletePrepared(
	ctx context.Context,
	registration biometric.Registration,
	storeOperationID string,
) (biometric.Snapshot, error) {
	if err := c.vault.Delete(ctx, registration); err != nil {
		return biometric.Snapshot{}, fmt.Errorf("%s: delete prepared template: %w", templateLifecycleOp, err)
	}
	snapshot, err := c.catalog.AbortPrepared(ctx, registration, storeOperationID)
	if err != nil {
		return biometric.Snapshot{}, fmt.Errorf("%s: abort prepared template: %w", templateLifecycleOp, err)
	}
	return snapshot, nil
}

func pendingRegistration(record biometric.Record) biometric.Registration {
	return biometric.Registration{
		ProfileRef: record.ProfileRef, Capability: record.Capability,
		TemplateRef: record.PendingStore.TemplateRef, ModelVersion: record.PendingStore.ModelVersion,
	}
}

func preparedStoreAuthorized(record biometric.Record) bool {
	return record.PendingStore != nil && record.Consented &&
		record.ConsentVersion == record.PendingStore.ConsentVersion &&
		record.PendingDelete == nil &&
		(record.Status == biometric.EnrollmentNone || record.Status == biometric.EnrollmentActive)
}

func enrollmentRecord(snapshot biometric.Snapshot, registration biometric.Registration) (biometric.Record, bool) {
	for _, record := range snapshot.Records {
		if record.ProfileRef == registration.ProfileRef && record.Capability == registration.Capability {
			return record, true
		}
	}
	return biometric.Record{}, false
}

func pendingMatches(pending *biometric.PendingTemplateReference, registration biometric.Registration) bool {
	return pending != nil && pending.TemplateRef == registration.TemplateRef && pending.ModelVersion == registration.ModelVersion
}

func wrapLifecycleError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %s: %w", templateLifecycleOp, action, err)
}

func descriptorForRegistration(registration biometric.Registration) Descriptor {
	return Descriptor{
		ProfileRef: registration.ProfileRef, Capability: registration.Capability,
		TemplateRef: registration.TemplateRef, ModelVersion: registration.ModelVersion,
	}
}
