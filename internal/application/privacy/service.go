package privacy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/domain/fault"
)

// Service serializes desired permission changes and publishes state only after
// durable persistence succeeds.
type Service struct {
	mu         sync.RWMutex
	repository Repository
	clock      port.Clock
	current    Snapshot
}

// New restores a persisted snapshot and fills every never-seen permission with
// the safe disabled default.
func New(ctx context.Context, repository Repository, clock port.Clock) (*Service, error) {
	const op = "create privacy permission service"
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if isNil(repository) || isNil(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("repository and clock are required"))
	}
	if err := ctx.Err(); err != nil {
		return nil, classify(op, err)
	}
	loaded, err := repository.Load(ctx)
	if err != nil {
		return nil, classify(op, err)
	}
	canonical, err := canonicalize(loaded)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, op, err)
	}
	return &Service{repository: repository, clock: clock, current: canonical}, nil
}

// Current returns an immutable copy without touching persistence.
func (s *Service) Current(ctx context.Context) (Snapshot, error) {
	const op = "read privacy permissions"
	if ctx == nil {
		return Snapshot{}, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, classify(op, err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.current), nil
}

// Change durably sets one permission before making it visible to readers.
func (s *Service) Change(ctx context.Context, command ChangePermission) (Snapshot, error) {
	const op = "change privacy permission"
	if ctx == nil {
		return Snapshot{}, fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, classify(op, err)
	}
	if !validPermission(command.Permission) {
		return Snapshot{}, fault.New(fault.InvalidInput, op, fmt.Errorf("unknown permission %q", command.Permission))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	index := grantIndex(s.current.Grants, command.Permission)
	if s.current.Grants[index].Enabled == command.Enabled {
		return cloneSnapshot(s.current), nil
	}

	next := cloneSnapshot(s.current)
	next.Revision++
	next.Grants[index].Enabled = command.Enabled
	next.Grants[index].UpdatedAt = s.clock.Now()
	if next.Grants[index].UpdatedAt.IsZero() {
		return Snapshot{}, fault.New(fault.InvalidInput, op, errors.New("clock returned zero time"))
	}
	if err := s.repository.Save(ctx, s.current.Revision, cloneSnapshot(next)); err != nil {
		return Snapshot{}, classify(op, err)
	}
	s.current = next
	return cloneSnapshot(s.current), nil
}

func canonicalize(snapshot Snapshot) (Snapshot, error) {
	if snapshot.Revision == 0 {
		for _, grant := range snapshot.Grants {
			if grant.Enabled || !grant.UpdatedAt.IsZero() {
				return Snapshot{}, errors.New("revision zero cannot contain changed grants")
			}
		}
	}
	byPermission := make(map[Permission]Grant, len(snapshot.Grants))
	for _, grant := range snapshot.Grants {
		if !validPermission(grant.Permission) {
			return Snapshot{}, fmt.Errorf("unknown permission %q", grant.Permission)
		}
		if _, exists := byPermission[grant.Permission]; exists {
			return Snapshot{}, fmt.Errorf("permission %q is duplicated", grant.Permission)
		}
		if grant.Enabled && grant.UpdatedAt.IsZero() {
			return Snapshot{}, fmt.Errorf("enabled permission %q has no update time", grant.Permission)
		}
		byPermission[grant.Permission] = grant
	}
	canonical := Snapshot{Revision: snapshot.Revision, Grants: make([]Grant, 0, len(permissionOrder))}
	for _, permission := range permissionOrder {
		grant, exists := byPermission[permission]
		if !exists {
			grant = Grant{Permission: permission}
		}
		canonical.Grants = append(canonical.Grants, grant)
	}
	return canonical, nil
}

func validPermission(permission Permission) bool {
	for _, candidate := range permissionOrder {
		if candidate == permission {
			return true
		}
	}
	return false
}

func grantIndex(grants []Grant, permission Permission) int {
	for index, grant := range grants {
		if grant.Permission == permission {
			return index
		}
	}
	return -1
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Grants = append([]Grant(nil), snapshot.Grants...)
	return snapshot
}

func classify(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fault.New(fault.DeadlineExceeded, op, err)
	}
	return fault.New(fault.Unavailable, op, err)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
