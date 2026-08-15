package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/runtime/provider"
)

// Spec is one explicitly configured local worker process.
type Spec struct {
	ProviderID string
	Permission privacy.Permission
	Command    string
	Args       []string
	WorkingDir string
	Env        []string
	// InstanceID is assigned by Supervisor for one process lifetime.
	// Composition specs must leave it empty.
	InstanceID string
}

// Process is a started child process with a single completion signal.
type Process interface {
	Signal(os.Signal) error
	Kill() error
	Done() <-chan error
}

// Launcher starts exactly the command declared by a Spec without a shell.
type Launcher interface {
	Start(Spec) (Process, error)
}

// LeaseSource is the immutable capability registry view used for health.
type LeaseSource interface {
	Subscribe() ([]readiness.ProviderSnapshot, <-chan []readiness.ProviderSnapshot, func())
	RevokeProvider(string) bool
}

// PermissionService is the persisted desired-state boundary consumed by the
// supervisor. Permission changes and process reconciliation remain separate.
type PermissionService interface {
	Current(context.Context) (privacy.Snapshot, error)
	Change(context.Context, privacy.ChangePermission) (privacy.Snapshot, error)
}

// Config bounds graceful worker termination.
type Config struct {
	StopTimeout         time.Duration
	HealthCheckInterval time.Duration
	after               func(time.Duration) <-chan time.Time
	newInstanceID       func(string) (string, error)
}

type controller struct {
	spec        Spec
	process     Process
	failed      bool
	everHealthy bool
	instanceID  string
	runtime     provider.Runtime
}

// Supervisor persists desired permissions, owns child processes, and exposes
// runtime health without equating permission with provider availability.
type Supervisor struct {
	config      Config
	permissions PermissionService
	leases      LeaseSource
	launcher    Launcher
	clock       port.Clock

	reconcile chan struct{}

	mu          sync.Mutex
	started     bool
	controllers []*controller
	leaseState  []readiness.ProviderSnapshot
	revision    uint64
	subs        map[uint64]chan provider.Snapshot
	nextSubID   uint64
}

// New constructs the explicit camera and microphone worker supervisor.
func New(config Config, permissions PermissionService, leases LeaseSource, launcher Launcher, clock port.Clock, specs []Spec) (*Supervisor, error) {
	const op = "create local worker supervisor"
	if config.StopTimeout <= 0 || config.HealthCheckInterval <= 0 || isNil(permissions) || isNil(leases) || isNil(launcher) || isNil(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("positive timeout and all dependencies are required"))
	}
	if len(specs) != 2 {
		return nil, fault.New(fault.InvalidInput, op, errors.New("camera and microphone worker specs are required"))
	}
	if config.after == nil {
		config.after = time.After
	}
	if config.newInstanceID == nil {
		config.newInstanceID = randomInstanceID
	}
	copied := append([]Spec(nil), specs...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].ProviderID < copied[j].ProviderID })
	seenProvider := make(map[string]struct{}, len(copied))
	seenPermission := make(map[privacy.Permission]struct{}, len(copied))
	now := clock.Now()
	if now.IsZero() {
		return nil, fault.New(fault.InvalidInput, op, errors.New("clock returned zero time"))
	}
	controllers := make([]*controller, 0, len(copied))
	for _, spec := range copied {
		if strings.TrimSpace(spec.ProviderID) == "" || spec.ProviderID != strings.TrimSpace(spec.ProviderID) || strings.TrimSpace(spec.Command) == "" || spec.InstanceID != "" {
			return nil, fault.New(fault.InvalidInput, op, errors.New("provider id and command are required and instance id must be supervisor-owned"))
		}
		if spec.Permission != privacy.CameraCapture && spec.Permission != privacy.MicrophoneCapture {
			return nil, fault.New(fault.InvalidInput, op, errors.New("only camera and microphone capture permissions may own workers"))
		}
		if _, exists := seenProvider[spec.ProviderID]; exists {
			return nil, fault.New(fault.InvalidInput, op, errors.New("provider id is duplicated"))
		}
		if _, exists := seenPermission[spec.Permission]; exists {
			return nil, fault.New(fault.InvalidInput, op, errors.New("worker permission is duplicated"))
		}
		seenProvider[spec.ProviderID] = struct{}{}
		seenPermission[spec.Permission] = struct{}{}
		spec.Args = append([]string(nil), spec.Args...)
		spec.Env = append([]string(nil), spec.Env...)
		controllers = append(controllers, &controller{spec: spec, runtime: provider.Runtime{
			ProviderID: spec.ProviderID, State: provider.Disabled, Reason: provider.ReasonDisabledByUser, UpdatedAt: now,
		}})
	}
	return &Supervisor{
		config: config, permissions: permissions, leases: leases, launcher: launcher, clock: clock,
		reconcile: make(chan struct{}, 1), controllers: controllers,
		revision: 1, subs: make(map[uint64]chan provider.Snapshot),
	}, nil
}

// Current delegates the persisted desired permission view.
func (s *Supervisor) Current(ctx context.Context) (privacy.Snapshot, error) {
	return s.permissions.Current(ctx)
}

// Change persists permission state before asynchronously reconciling workers.
func (s *Supervisor) Change(ctx context.Context, command privacy.ChangePermission) (privacy.Snapshot, error) {
	snapshot, err := s.permissions.Change(ctx, command)
	if err != nil {
		return privacy.Snapshot{}, err
	}
	if !command.Enabled {
		s.mu.Lock()
		for _, item := range s.controllers {
			if item.spec.Permission == command.Permission {
				item.failed = false
				break
			}
		}
		s.mu.Unlock()
	}
	s.signal(s.reconcile)
	return snapshot, nil
}

// Run owns all child-process transitions and joins them before returning.
func (s *Supervisor) Run(ctx context.Context) error {
	const op = "run local worker supervisor"
	if ctx == nil {
		return fault.New(fault.InvalidInput, op, errors.New("context is required"))
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fault.New(fault.InvalidInput, op, errors.New("supervisor may only run once"))
	}
	s.started = true
	s.mu.Unlock()

	initial, leaseUpdates, cancelLeases := s.leases.Subscribe()
	defer cancelLeases()
	healthChecks := s.clock.NewTicker(s.config.HealthCheckInterval)
	defer healthChecks.Stop()
	s.setLeases(initial)
	if err := s.reconcileAll(ctx); err != nil {
		return err
	}
	for {
		firstProcessDone, secondProcessDone := s.processDoneChannels()
		select {
		case <-ctx.Done():
			return s.stopAll()
		case <-s.reconcile:
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		case snapshots, ok := <-leaseUpdates:
			if !ok {
				return errors.Join(fault.New(fault.Unavailable, op, errors.New("capability lease source closed")), s.stopAll())
			}
			s.setLeases(snapshots)
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		case <-firstProcessDone:
			s.noteProcessExit(s.controllers[0])
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		case <-secondProcessDone:
			s.noteProcessExit(s.controllers[1])
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		case <-healthChecks.C():
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		}
	}
}

func (s *Supervisor) reconcileAll(ctx context.Context) error {
	permissions, err := s.permissions.Current(ctx)
	if err != nil {
		return err
	}
	for _, item := range s.controllers {
		if err := s.reconcileOne(item, permissionEnabled(permissions, item.spec.Permission)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) reconcileOne(item *controller, enabled bool) error {
	s.mu.Lock()
	process := item.process
	if process != nil {
		select {
		case <-process.Done():
			item.process = nil
			item.instanceID = ""
			item.everHealthy = false
			process = nil
			s.leases.RevokeProvider(item.spec.ProviderID)
			s.removeLeaseStateLocked(item.spec.ProviderID)
			if enabled {
				item.failed = true
				s.setRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
			}
		default:
		}
	}
	if !enabled {
		item.failed = false
		if process == nil {
			item.instanceID = ""
			item.everHealthy = false
			s.setRuntimeLocked(item, provider.Disabled, provider.ReasonDisabledByUser)
			s.mu.Unlock()
			return nil
		}
		s.setRuntimeLocked(item, provider.Stopping, provider.ReasonShuttingDown)
		s.mu.Unlock()
		stopErr := s.stopProcess(process)
		s.leases.RevokeProvider(item.spec.ProviderID)
		s.mu.Lock()
		s.removeLeaseStateLocked(item.spec.ProviderID)
		if stopErr != nil {
			s.setRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
			s.mu.Unlock()
			return stopErr
		}
		item.process = nil
		item.instanceID = ""
		item.everHealthy = false
		s.setRuntimeLocked(item, provider.Disabled, provider.ReasonDisabledByUser)
		s.mu.Unlock()
		return nil
	}
	if process == nil {
		if item.failed {
			s.mu.Unlock()
			return nil
		}
		s.setRuntimeLocked(item, provider.Starting, provider.ReasonNone)
		instanceID, err := s.config.newInstanceID(item.spec.ProviderID)
		if err != nil || strings.TrimSpace(instanceID) == "" || instanceID != strings.TrimSpace(instanceID) {
			item.failed = true
			s.setRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
			s.mu.Unlock()
			return nil
		}
		spec := item.spec
		spec.InstanceID = instanceID
		s.mu.Unlock()
		started, err := s.launcher.Start(spec)
		s.mu.Lock()
		if err != nil || isNil(started) {
			item.failed = true
			s.setRuntimeLocked(item, provider.Degraded, provider.ReasonDependencyUnavailable)
			s.mu.Unlock()
			return nil
		}
		item.process = started
		item.instanceID = instanceID
		item.everHealthy = false
		process = started
	}
	s.refreshLeaseStateLocked(item)
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) processDoneChannels() (<-chan error, <-chan error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first, second <-chan error
	if s.controllers[0].process != nil {
		first = s.controllers[0].process.Done()
	}
	if s.controllers[1].process != nil {
		second = s.controllers[1].process.Done()
	}
	return first, second
}

func (s *Supervisor) noteProcessExit(item *controller) {
	s.mu.Lock()
	if item.process == nil {
		s.mu.Unlock()
		return
	}
	item.process = nil
	item.instanceID = ""
	item.everHealthy = false
	item.failed = true
	s.removeLeaseStateLocked(item.spec.ProviderID)
	s.setRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
	s.mu.Unlock()
	s.leases.RevokeProvider(item.spec.ProviderID)
}

func (s *Supervisor) refreshLeaseStateLocked(item *controller) {
	now := s.clock.Now()
	for _, snapshot := range s.leaseState {
		if snapshot.ProviderID != item.spec.ProviderID || snapshot.InstanceID != item.instanceID {
			continue
		}
		item.everHealthy = true
		if !now.Before(snapshot.LeaseExpiresAt) {
			s.setRuntimeLocked(item, provider.Degraded, provider.ReasonDeviceUnavailable)
			return
		}
		state, reason := providerRuntimeHealth(snapshot.Health, snapshot.HealthReason)
		s.setRuntimeLocked(item, state, reason)
		return
	}
	if item.everHealthy {
		s.setRuntimeLocked(item, provider.Degraded, provider.ReasonDeviceUnavailable)
		return
	}
	s.setRuntimeLocked(item, provider.Starting, provider.ReasonNone)
}

func providerRuntimeHealth(health readiness.ProviderHealth, reason readiness.ProviderHealthReason) (provider.State, provider.Reason) {
	if health == readiness.Healthy && reason == readiness.ProviderHealthReasonNone {
		return provider.Running, provider.ReasonNone
	}
	if health != readiness.Unhealthy {
		return provider.Degraded, provider.ReasonInternalError
	}
	switch reason {
	case readiness.ProviderHealthReasonStarting:
		return provider.Starting, provider.ReasonNone
	case readiness.ProviderHealthReasonDeviceUnavailable:
		return provider.Degraded, provider.ReasonDeviceUnavailable
	case readiness.ProviderHealthReasonPermissionDenied:
		return provider.Degraded, provider.ReasonPermissionDenied
	case readiness.ProviderHealthReasonDependencyUnavailable:
		return provider.Degraded, provider.ReasonDependencyUnavailable
	case readiness.ProviderHealthReasonModelUnavailable:
		return provider.Degraded, provider.ReasonModelUnavailable
	case readiness.ProviderHealthReasonInternalError:
		return provider.Degraded, provider.ReasonInternalError
	case readiness.ProviderHealthReasonShuttingDown:
		return provider.Stopping, provider.ReasonShuttingDown
	default:
		return provider.Degraded, provider.ReasonInternalError
	}
}

func (s *Supervisor) stopAll() error {
	var stopErrors []error
	for _, item := range s.controllers {
		s.mu.Lock()
		process := item.process
		var stopErr error
		if process != nil {
			s.setRuntimeLocked(item, provider.Stopping, provider.ReasonShuttingDown)
		}
		s.mu.Unlock()
		if process != nil {
			if stopErr = s.stopProcess(process); stopErr != nil {
				stopErrors = append(stopErrors, stopErr)
			}
			s.leases.RevokeProvider(item.spec.ProviderID)
		}
		s.mu.Lock()
		s.removeLeaseStateLocked(item.spec.ProviderID)
		if stopErr != nil {
			s.setRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
		} else {
			item.process = nil
			item.instanceID = ""
			item.failed = false
			item.everHealthy = false
			s.setRuntimeLocked(item, provider.Disabled, provider.ReasonDisabledByUser)
		}
		s.mu.Unlock()
	}
	return errors.Join(stopErrors...)
}

func (s *Supervisor) stopProcess(process Process) error {
	const op = "stop local worker process"
	_ = process.Signal(syscall.SIGTERM)
	select {
	case <-process.Done():
		return nil
	case <-s.config.after(s.config.StopTimeout):
	}
	if err := process.Kill(); err != nil {
		return fault.New(fault.Unavailable, op, err)
	}
	select {
	case <-process.Done():
		return nil
	case <-s.config.after(s.config.StopTimeout):
		return fault.New(fault.DeadlineExceeded, op, errors.New("process did not exit after kill"))
	}
}

func (s *Supervisor) setLeases(input []readiness.ProviderSnapshot) {
	s.mu.Lock()
	s.leaseState = cloneLeases(input)
	s.mu.Unlock()
}

func (s *Supervisor) removeLeaseStateLocked(providerID string) {
	filtered := s.leaseState[:0]
	for _, snapshot := range s.leaseState {
		if snapshot.ProviderID != providerID {
			filtered = append(filtered, snapshot)
		}
	}
	s.leaseState = filtered
}

func (s *Supervisor) setRuntimeLocked(item *controller, state provider.State, reason provider.Reason) {
	if item.runtime.State == state && item.runtime.Reason == reason {
		return
	}
	item.runtime.State = state
	item.runtime.Reason = reason
	item.runtime.UpdatedAt = s.clock.Now()
	s.revision++
	s.notifyLocked()
}

// CurrentProviderRuntime returns a value copy in stable provider order.
func (s *Supervisor) CurrentProviderRuntime() provider.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// SubscribeProviderRuntime returns a latest-only immutable status stream.
func (s *Supervisor) SubscribeProviderRuntime() (provider.Snapshot, <-chan provider.Snapshot, func()) {
	s.mu.Lock()
	id := s.nextSubID
	s.nextSubID++
	updates := make(chan provider.Snapshot, 1)
	s.subs[id] = updates
	initial := s.snapshotLocked()
	s.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if existing, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(existing)
			}
			s.mu.Unlock()
		})
	}
	return initial, updates, cancel
}

func (s *Supervisor) snapshotLocked() provider.Snapshot {
	snapshot := provider.Snapshot{Revision: s.revision, Providers: make([]provider.Runtime, 0, len(s.controllers))}
	for _, item := range s.controllers {
		snapshot.Providers = append(snapshot.Providers, item.runtime)
	}
	return snapshot
}

func (s *Supervisor) notifyLocked() {
	for _, subscriber := range s.subs {
		snapshot := s.snapshotLocked()
		select {
		case subscriber <- snapshot:
		default:
			select {
			case <-subscriber:
			default:
			}
			subscriber <- snapshot
		}
	}
}

func (s *Supervisor) signal(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

func permissionEnabled(snapshot privacy.Snapshot, permission privacy.Permission) bool {
	for _, grant := range snapshot.Grants {
		if grant.Permission == permission {
			return grant.Enabled
		}
	}
	return false
}

func cloneLeases(input []readiness.ProviderSnapshot) []readiness.ProviderSnapshot {
	output := append([]readiness.ProviderSnapshot(nil), input...)
	for index := range output {
		output[index].Capabilities = append([]readiness.CapabilityKind(nil), output[index].Capabilities...)
	}
	return output
}

func randomInstanceID(providerID string) (string, error) {
	const op = "create local worker instance id"
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fault.New(fault.Unavailable, op, err)
	}
	return providerID + "-" + hex.EncodeToString(buffer), nil
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
