package biometric

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"proactive-interaction-engine/internal/domain/fault"
)

const deletionCoordinatorOp = "coordinate biometric template deletion"

// TemplateDeleter physically deletes one opaque template identified only by
// catalog metadata. Template bytes never cross this application port.
type TemplateDeleter interface {
	Delete(context.Context, Registration) error
}

// DeletionCoordinator makes catalog state fail closed before physical template
// deletion and confirms metadata removal only after the deleter succeeds.
type DeletionCoordinator struct {
	mu      sync.Mutex
	catalog *Service
	deleter TemplateDeleter
}

// NewDeletionCoordinator constructs the dedicated template deletion use case.
func NewDeletionCoordinator(catalog *Service, deleter TemplateDeleter) (*DeletionCoordinator, error) {
	if isNil(catalog) || isNil(deleter) {
		return nil, fault.New(fault.InvalidInput, deletionCoordinatorOp, errors.New("catalog and template deleter are required"))
	}
	return &DeletionCoordinator{catalog: catalog, deleter: deleter}, nil
}

// Delete makes one enrollment unusable before attempting physical deletion.
// A failed delete or confirmation remains durably pending for a later retry.
func (c *DeletionCoordinator) Delete(ctx context.Context, key EnrollmentKey) (Snapshot, error) {
	if err := validateDeletionContext(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateKey(key.ProfileRef, key.Capability); err != nil {
		return Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, err := c.catalog.RequestDelete(ctx, key)
	if err != nil {
		return Snapshot{}, wrapDeletionFault("request deletion", Registration{
			ProfileRef: key.ProfileRef, Capability: key.Capability,
		}, err)
	}
	return c.deletePending(ctx, snapshot, func(record Record) bool {
		return record.ProfileRef == key.ProfileRef && record.Capability == key.Capability
	})
}

// DeleteProfile revokes all profile consent, durably marks active enrollments
// for deletion, and attempts every current and retired template in stable order.
func (c *DeletionCoordinator) DeleteProfile(ctx context.Context, profileRef string) (Snapshot, error) {
	if err := validateDeletionContext(ctx); err != nil {
		return Snapshot{}, err
	}
	if !validString(profileRef) {
		return Snapshot{}, invalidInput("profile reference is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshot, err := c.catalog.DeleteProfile(ctx, profileRef)
	if err != nil {
		return Snapshot{}, wrapDeletionFault("request profile deletion", Registration{ProfileRef: profileRef}, err)
	}
	return c.deletePending(ctx, snapshot, func(record Record) bool {
		return record.ProfileRef == profileRef
	})
}

// RetryPendingDeletes retries every durably pending current and retired
// template in canonical catalog order.
func (c *DeletionCoordinator) RetryPendingDeletes(ctx context.Context) (Snapshot, error) {
	if err := validateDeletionContext(ctx); err != nil {
		return Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deletePending(ctx, c.catalog.Current(), func(Record) bool { return true })
}

func (c *DeletionCoordinator) deletePending(ctx context.Context, snapshot Snapshot, include func(Record) bool) (Snapshot, error) {
	var firstErr error
	remember := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, record := range snapshot.Records {
		if !include(record) {
			continue
		}
		key := EnrollmentKey{ProfileRef: record.ProfileRef, Capability: record.Capability}
		if record.Status == EnrollmentDeletePending {
			current := Registration{
				ProfileRef: record.ProfileRef, Capability: record.Capability,
				TemplateRef: record.TemplateRef, ModelVersion: record.ModelVersion,
			}
			if err := c.deleter.Delete(ctx, current); err != nil {
				remember(wrapDeletionFault("delete current template", current, err))
			} else if _, err := c.catalog.ConfirmDelete(ctx, key); err != nil {
				remember(wrapDeletionFault("confirm current template deletion", current, err))
			}
		}

		if record.PendingDelete == nil {
			continue
		}
		retired := Registration{
			ProfileRef: record.ProfileRef, Capability: record.Capability,
			TemplateRef: record.PendingDelete.TemplateRef, ModelVersion: record.PendingDelete.ModelVersion,
		}
		if err := c.deleter.Delete(ctx, retired); err != nil {
			remember(wrapDeletionFault("delete retired template", retired, err))
			continue
		}
		if _, err := c.catalog.ConfirmRetiredDelete(ctx, key, *record.PendingDelete); err != nil {
			remember(wrapDeletionFault("confirm retired template deletion", retired, err))
		}
	}
	return c.catalog.Current(), firstErr
}

func validateDeletionContext(ctx context.Context) error {
	if ctx == nil {
		return fault.New(fault.InvalidInput, deletionCoordinatorOp, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return classify(deletionCoordinatorOp, err)
	}
	return nil
}

func wrapDeletionFault(action string, registration Registration, err error) error {
	classified := classify(deletionCoordinatorOp, err)
	return fmt.Errorf(
		"%s for profile %q capability %q template %q model %q: %w",
		action, registration.ProfileRef, registration.Capability,
		registration.TemplateRef, registration.ModelVersion, classified,
	)
}
