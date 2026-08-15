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

	"proactive-interaction-engine/internal/application/biometric"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/privacy"
	"proactive-interaction-engine/internal/application/readiness"
	"proactive-interaction-engine/internal/domain/fault"
	"proactive-interaction-engine/internal/runtime/provider"
)

const maxSupervisorIDBytes = 256

// LogicalProviderSpec binds one globally unique provider to exactly one
// capability implemented inside its device process group.
type LogicalProviderSpec struct {
	ProviderID string
	Capability readiness.CapabilityKind
}

// ProcessSpec is one explicitly configured camera or microphone process group.
// InstanceID is assigned by Supervisor for one process lifetime; composition
// specs must leave it empty.
type ProcessSpec struct {
	ProcessID      string
	BasePermission privacy.Permission
	Command        string
	Args           []string
	Env            []string
	WorkingDir     string
	InstanceID     string
	Providers      []LogicalProviderSpec
}

// Process is a started child process with a single completion signal.
type Process interface {
	Signal(os.Signal) error
	Kill() error
	Done() <-chan error
}

// Launcher starts exactly the command declared by a ProcessSpec without a
// shell. Providers contains only the logical providers authorized for that
// process lifetime.
type Launcher interface {
	Start(ProcessSpec) (Process, error)
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

type logicalController struct {
	spec        LogicalProviderSpec
	runtime     provider.Runtime
	everHealthy bool
}

type controller struct {
	spec                 ProcessSpec
	providers            []*logicalController
	process              Process
	failed               bool
	reconfigure          bool
	instanceID           string
	effective            []LogicalProviderSpec
	effectiveInitialized bool
}

// Supervisor persists desired permissions, owns exactly two device process
// groups, and exposes independent runtime health for every logical provider.
type Supervisor struct {
	config      Config
	permissions PermissionService
	leases      LeaseSource
	launcher    Launcher
	clock       port.Clock

	reconcile chan struct{}

	permissionTransitions sync.Mutex
	permissionSettled     *sync.Cond
	permissionChanges     int
	mu                    sync.Mutex
	started               bool
	controllers           []*controller
	leaseState            []readiness.ProviderSnapshot
	revision              uint64
	subs                  map[uint64]chan provider.Snapshot
	nextSubID             uint64
}

// New constructs one camera and one microphone device process group.
func New(config Config, permissions PermissionService, leases LeaseSource, launcher Launcher, clock port.Clock, specs []ProcessSpec) (*Supervisor, error) {
	const op = "create local worker supervisor"
	if config.StopTimeout <= 0 || config.HealthCheckInterval <= 0 || isNil(permissions) || isNil(leases) || isNil(launcher) || isNil(clock) {
		return nil, fault.New(fault.InvalidInput, op, errors.New("positive timeout and all dependencies are required"))
	}
	if len(specs) != 2 {
		return nil, fault.New(fault.InvalidInput, op, errors.New("exactly one camera and one microphone process group are required"))
	}
	if config.after == nil {
		config.after = time.After
	}
	if config.newInstanceID == nil {
		config.newInstanceID = randomInstanceID
	}
	now := clock.Now()
	if now.IsZero() {
		return nil, fault.New(fault.InvalidInput, op, errors.New("clock returned zero time"))
	}

	copied := cloneProcessSpecs(specs)
	sort.Slice(copied, func(i, j int) bool { return copied[i].ProcessID < copied[j].ProcessID })
	seenIDs := make(map[string]struct{})
	seenPermissions := make(map[privacy.Permission]struct{}, 2)
	for _, spec := range copied {
		if !validID(spec.ProcessID) || strings.TrimSpace(spec.Command) == "" || spec.InstanceID != "" {
			return nil, fault.New(fault.InvalidInput, op, errors.New("process id and command are required and instance id must be supervisor-owned"))
		}
		if _, duplicate := seenIDs[spec.ProcessID]; duplicate {
			return nil, fault.New(fault.InvalidInput, op, errors.New("process or provider id is duplicated"))
		}
		seenIDs[spec.ProcessID] = struct{}{}
		if spec.BasePermission != privacy.CameraCapture && spec.BasePermission != privacy.MicrophoneCapture {
			return nil, fault.New(fault.InvalidInput, op, errors.New("only camera and microphone capture permissions may own process groups"))
		}
		if _, duplicate := seenPermissions[spec.BasePermission]; duplicate {
			return nil, fault.New(fault.InvalidInput, op, errors.New("device process group permission is duplicated"))
		}
		seenPermissions[spec.BasePermission] = struct{}{}
	}

	controllers := make([]*controller, 0, len(copied))
	for _, spec := range copied {
		if len(spec.Providers) == 0 {
			return nil, fault.New(fault.InvalidInput, op, errors.New("each process group requires logical providers"))
		}
		seenCapabilities := make(map[readiness.CapabilityKind]struct{}, len(spec.Providers))
		hasBase := false
		logical := make([]*logicalController, 0, len(spec.Providers))
		for _, providerSpec := range spec.Providers {
			if !validID(providerSpec.ProviderID) {
				return nil, fault.New(fault.InvalidInput, op, errors.New("logical provider id is required"))
			}
			if _, duplicate := seenIDs[providerSpec.ProviderID]; duplicate {
				return nil, fault.New(fault.InvalidInput, op, errors.New("process or provider id is duplicated"))
			}
			seenIDs[providerSpec.ProviderID] = struct{}{}
			if _, duplicate := seenCapabilities[providerSpec.Capability]; duplicate {
				return nil, fault.New(fault.InvalidInput, op, errors.New("logical capability is duplicated within a process group"))
			}
			seenCapabilities[providerSpec.Capability] = struct{}{}
			if !capabilityAllowed(spec.BasePermission, providerSpec.Capability) {
				return nil, fault.New(fault.InvalidInput, op, errors.New("logical capability does not belong to its device process group"))
			}
			if providerSpec.Capability == baseCapability(spec.BasePermission) {
				hasBase = true
			}
			reason := provider.ReasonPermissionDenied
			if providerSpec.Capability == baseCapability(spec.BasePermission) {
				reason = provider.ReasonDisabledByUser
			}
			logical = append(logical, &logicalController{spec: providerSpec, runtime: provider.Runtime{
				ProviderID: providerSpec.ProviderID, State: provider.Disabled, Reason: reason, UpdatedAt: now,
			}})
		}
		if !hasBase {
			return nil, fault.New(fault.InvalidInput, op, errors.New("device process group is missing its base capability"))
		}
		sort.Slice(spec.Providers, func(i, j int) bool { return spec.Providers[i].ProviderID < spec.Providers[j].ProviderID })
		sort.Slice(logical, func(i, j int) bool { return logical[i].spec.ProviderID < logical[j].spec.ProviderID })
		controllers = append(controllers, &controller{spec: spec, providers: logical})
	}
	if len(seenPermissions) != 2 {
		return nil, fault.New(fault.InvalidInput, op, errors.New("camera and microphone process groups are both required"))
	}
	supervisor := &Supervisor{
		config: config, permissions: permissions, leases: leases, launcher: launcher, clock: clock,
		reconcile: make(chan struct{}, 1), controllers: controllers,
		revision: 1, subs: make(map[uint64]chan provider.Snapshot),
	}
	supervisor.permissionSettled = sync.NewCond(&supervisor.permissionTransitions)
	return supervisor, nil
}

// Current delegates the persisted desired permission view.
func (s *Supervisor) Current(ctx context.Context) (privacy.Snapshot, error) {
	return s.permissions.Current(ctx)
}

// Change persists permission state before asynchronously reconciling process
// groups. An idempotent change cannot retry a failed process because its
// effective provider set is unchanged.
func (s *Supervisor) Change(ctx context.Context, command privacy.ChangePermission) (privacy.Snapshot, error) {
	s.beginPermissionChange()
	defer s.finishPermissionChange()
	snapshot, err := s.permissions.Change(ctx, command)
	if err != nil {
		return privacy.Snapshot{}, err
	}
	desired := make([][]LogicalProviderSpec, len(s.controllers))
	for index, item := range s.controllers {
		desired[index], err = effectiveProviders(snapshot, item.spec)
		if err != nil {
			break
		}
	}
	if err == nil {
		s.mu.Lock()
		for index, item := range s.controllers {
			if item.effectiveInitialized && !sameProviders(item.effective, desired[index]) {
				item.reconfigure = true
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
		case <-secondProcessDone:
			s.noteProcessExit(s.controllers[1])
		case <-healthChecks.C():
			if err := s.reconcileAll(ctx); err != nil {
				return errors.Join(err, s.stopAll())
			}
		}
	}
}

func (s *Supervisor) reconcileAll(ctx context.Context) error {
	snapshot, err := s.permissions.Current(ctx)
	if err != nil {
		return err
	}
	desired := make([][]LogicalProviderSpec, len(s.controllers))
	for index, item := range s.controllers {
		desired[index], err = effectiveProviders(snapshot, item.spec)
		if err != nil {
			return err
		}
	}
	for index, item := range s.controllers {
		if err := s.reconcileOne(ctx, item, desired[index]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) reconcileOne(ctx context.Context, item *controller, desired []LogicalProviderSpec) error {
	permissionLocked := false
	defer func() {
		if permissionLocked {
			s.permissionTransitions.Unlock()
		}
	}()
	if s.processExited(item) {
		s.noteProcessExit(item)
	}

	s.mu.Lock()
	changed := item.effectiveInitialized && (item.reconfigure || !sameProviders(item.effective, desired))
	initial := !item.effectiveInitialized
	if initial {
		item.effectiveInitialized = true
		item.effective = cloneLogicalProviders(desired)
		s.applyAuthorizationLocked(item)
	} else if changed {
		process := item.process
		item.reconfigure = false
		if process != nil {
			s.setEnabledRuntimeLocked(item, provider.Stopping, provider.ReasonShuttingDown)
		}
		s.mu.Unlock()

		var stopErr error
		if process != nil {
			stopErr = s.stopProcess(process)
		}
		processStopped := stopErr == nil
		s.revokeGroup(item)
		if stopErr == nil {
			s.lockStablePermissions()
			permissionLocked = true
			latest, currentErr := s.permissions.Current(ctx)
			if currentErr != nil {
				stopErr = currentErr
			} else {
				desired, currentErr = effectiveProviders(latest, item.spec)
				if currentErr != nil {
					stopErr = currentErr
				}
			}
		}

		s.mu.Lock()
		s.removeGroupLeaseStateLocked(item)
		for _, logical := range item.providers {
			logical.everHealthy = false
		}
		item.effective = cloneLogicalProviders(desired)
		item.reconfigure = false
		item.instanceID = ""
		if stopErr != nil {
			if processStopped {
				item.process = nil
			}
			item.failed = true
			s.setEnabledRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
			s.applyDisabledAuthorizationLocked(item)
			s.mu.Unlock()
			return stopErr
		}
		item.process = nil
		item.failed = false
		s.applyAuthorizationLocked(item)
	}

	if len(item.effective) == 0 {
		s.applyAuthorizationLocked(item)
		s.mu.Unlock()
		return nil
	}
	if item.process == nil {
		if item.failed {
			s.applyDisabledAuthorizationLocked(item)
			s.mu.Unlock()
			return nil
		}
		if !permissionLocked {
			s.mu.Unlock()
			s.lockStablePermissions()
			permissionLocked = true
			latest, currentErr := s.permissions.Current(ctx)
			if currentErr != nil {
				return currentErr
			}
			latestDesired, currentErr := effectiveProviders(latest, item.spec)
			if currentErr != nil {
				return currentErr
			}
			s.mu.Lock()
			item.effective = cloneLogicalProviders(latestDesired)
			item.reconfigure = false
			s.applyAuthorizationLocked(item)
			if len(item.effective) == 0 {
				s.mu.Unlock()
				return nil
			}
		}
		s.setEnabledRuntimeLocked(item, provider.Starting, provider.ReasonNone)
		instanceID, err := s.config.newInstanceID(item.spec.ProcessID)
		if err != nil || !validID(instanceID) {
			item.failed = true
			s.setEnabledRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
			s.mu.Unlock()
			return nil
		}
		spec := cloneProcessSpec(item.spec)
		spec.InstanceID = instanceID
		spec.Providers = cloneLogicalProviders(item.effective)
		s.mu.Unlock()

		started, startErr := s.launcher.Start(spec)
		s.mu.Lock()
		if startErr != nil || isNil(started) {
			item.failed = true
			s.setEnabledRuntimeLocked(item, provider.Degraded, provider.ReasonDependencyUnavailable)
			s.mu.Unlock()
			return nil
		}
		item.process = started
		item.instanceID = instanceID
	}
	s.refreshLeaseStateLocked(item)
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) processExited(item *controller) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.process == nil {
		return false
	}
	select {
	case <-item.process.Done():
		return true
	default:
		return false
	}
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
	item.failed = true
	for _, logical := range item.providers {
		logical.everHealthy = false
	}
	s.removeGroupLeaseStateLocked(item)
	s.setEnabledRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
	s.applyDisabledAuthorizationLocked(item)
	s.mu.Unlock()
	s.revokeGroup(item)
}

func (s *Supervisor) refreshLeaseStateLocked(item *controller) {
	now := s.clock.Now()
	enabled := providerIDSet(item.effective)
	for _, logical := range item.providers {
		if _, ok := enabled[logical.spec.ProviderID]; !ok {
			s.setUnauthorizedRuntimeLocked(item, logical)
			continue
		}
		matched := false
		for _, snapshot := range s.leaseState {
			if snapshot.ProviderID != logical.spec.ProviderID || snapshot.InstanceID != item.instanceID {
				continue
			}
			matched = true
			logical.everHealthy = true
			if len(snapshot.Capabilities) != 1 || snapshot.Capabilities[0] != logical.spec.Capability {
				s.setRuntimeLocked(logical, provider.Degraded, provider.ReasonInternalError)
				break
			}
			if !now.Before(snapshot.LeaseExpiresAt) {
				s.setRuntimeLocked(logical, provider.Degraded, provider.ReasonDeviceUnavailable)
				break
			}
			state, reason := providerRuntimeHealth(snapshot.Health, snapshot.HealthReason)
			s.setRuntimeLocked(logical, state, reason)
			break
		}
		if matched {
			continue
		}
		if logical.everHealthy {
			s.setRuntimeLocked(logical, provider.Degraded, provider.ReasonDeviceUnavailable)
			continue
		}
		s.setRuntimeLocked(logical, provider.Starting, provider.ReasonNone)
	}
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
		if process != nil {
			s.setEnabledRuntimeLocked(item, provider.Stopping, provider.ReasonShuttingDown)
		}
		s.mu.Unlock()

		var stopErr error
		if process != nil {
			stopErr = s.stopProcess(process)
			if stopErr != nil {
				stopErrors = append(stopErrors, stopErr)
			}
		}
		s.revokeGroup(item)

		s.mu.Lock()
		s.removeGroupLeaseStateLocked(item)
		for _, logical := range item.providers {
			logical.everHealthy = false
		}
		if stopErr != nil {
			s.setEnabledRuntimeLocked(item, provider.Degraded, provider.ReasonInternalError)
		} else {
			item.process = nil
			item.instanceID = ""
			item.failed = false
			item.effective = nil
			for _, logical := range item.providers {
				reason := provider.ReasonPermissionDenied
				if logical.spec.Capability == baseCapability(item.spec.BasePermission) {
					reason = provider.ReasonDisabledByUser
				}
				s.setRuntimeLocked(logical, provider.Disabled, reason)
			}
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

func (s *Supervisor) applyAuthorizationLocked(item *controller) {
	enabled := providerIDSet(item.effective)
	for _, logical := range item.providers {
		if _, ok := enabled[logical.spec.ProviderID]; ok {
			s.setRuntimeLocked(logical, provider.Starting, provider.ReasonNone)
			continue
		}
		s.setUnauthorizedRuntimeLocked(item, logical)
	}
}

func (s *Supervisor) applyDisabledAuthorizationLocked(item *controller) {
	enabled := providerIDSet(item.effective)
	for _, logical := range item.providers {
		if _, ok := enabled[logical.spec.ProviderID]; !ok {
			s.setUnauthorizedRuntimeLocked(item, logical)
		}
	}
}

func (s *Supervisor) setUnauthorizedRuntimeLocked(item *controller, logical *logicalController) {
	reason := provider.ReasonPermissionDenied
	if logical.spec.Capability == baseCapability(item.spec.BasePermission) {
		reason = provider.ReasonDisabledByUser
	}
	s.setRuntimeLocked(logical, provider.Disabled, reason)
}

func (s *Supervisor) setEnabledRuntimeLocked(item *controller, state provider.State, reason provider.Reason) {
	enabled := providerIDSet(item.effective)
	for _, logical := range item.providers {
		if _, ok := enabled[logical.spec.ProviderID]; ok {
			s.setRuntimeLocked(logical, state, reason)
		}
	}
}

func (s *Supervisor) setLeases(input []readiness.ProviderSnapshot) {
	s.mu.Lock()
	s.leaseState = cloneLeases(input)
	s.mu.Unlock()
}

func (s *Supervisor) revokeGroup(item *controller) {
	for _, logical := range item.providers {
		s.leases.RevokeProvider(logical.spec.ProviderID)
	}
}

func (s *Supervisor) removeGroupLeaseStateLocked(item *controller) {
	providerIDs := make(map[string]struct{}, len(item.providers))
	for _, logical := range item.providers {
		providerIDs[logical.spec.ProviderID] = struct{}{}
	}
	filtered := s.leaseState[:0]
	for _, snapshot := range s.leaseState {
		if _, remove := providerIDs[snapshot.ProviderID]; !remove {
			filtered = append(filtered, snapshot)
		}
	}
	s.leaseState = filtered
}

func (s *Supervisor) setRuntimeLocked(item *logicalController, state provider.State, reason provider.Reason) {
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
	providers := make([]provider.Runtime, 0)
	for _, item := range s.controllers {
		for _, logical := range item.providers {
			providers = append(providers, logical.runtime)
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ProviderID < providers[j].ProviderID })
	return provider.Snapshot{Revision: s.revision, Providers: providers}
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

func (s *Supervisor) beginPermissionChange() {
	s.permissionTransitions.Lock()
	s.permissionChanges++
	s.permissionTransitions.Unlock()
}

func (s *Supervisor) finishPermissionChange() {
	s.permissionTransitions.Lock()
	s.permissionChanges--
	s.permissionSettled.Broadcast()
	s.permissionTransitions.Unlock()
}

// lockStablePermissions waits for already-started permission persistence to
// finish and blocks a new change from beginning until the caller unlocks
// permissionTransitions. This makes the final permission read and synchronous
// process launch one authorization boundary without serializing Change calls
// against each other.
func (s *Supervisor) lockStablePermissions() {
	s.permissionTransitions.Lock()
	for s.permissionChanges > 0 {
		s.permissionSettled.Wait()
	}
}

func effectiveProviders(snapshot privacy.Snapshot, spec ProcessSpec) ([]LogicalProviderSpec, error) {
	output := make([]LogicalProviderSpec, 0, len(spec.Providers))
	for _, logical := range spec.Providers {
		var enabled bool
		if logical.Capability == baseCapability(spec.BasePermission) {
			enabled = permissionEnabled(snapshot, spec.BasePermission)
		} else {
			var err error
			enabled, err = biometric.CapabilityAuthorized(snapshot, logical.Capability)
			if err != nil {
				return nil, err
			}
		}
		if enabled {
			output = append(output, logical)
		}
	}
	sort.Slice(output, func(i, j int) bool { return output[i].ProviderID < output[j].ProviderID })
	return output, nil
}

func permissionEnabled(snapshot privacy.Snapshot, permission privacy.Permission) bool {
	for _, grant := range snapshot.Grants {
		if grant.Permission == permission {
			return grant.Enabled
		}
	}
	return false
}

func baseCapability(permission privacy.Permission) readiness.CapabilityKind {
	if permission == privacy.CameraCapture {
		return readiness.PersonPresence
	}
	if permission == privacy.MicrophoneCapture {
		return readiness.VoiceActivity
	}
	return ""
}

func capabilityAllowed(permission privacy.Permission, capability readiness.CapabilityKind) bool {
	switch permission {
	case privacy.CameraCapture:
		return capability == readiness.PersonPresence || capability == readiness.FaceDetection || capability == readiness.FaceIdentification || capability == readiness.FaceLiveness
	case privacy.MicrophoneCapture:
		return capability == readiness.VoiceActivity || capability == readiness.SpeakerIdentification || capability == readiness.SpeakerVerification
	default:
		return false
	}
}

func sameProviders(first, second []LogicalProviderSpec) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func providerIDSet(input []LogicalProviderSpec) map[string]struct{} {
	output := make(map[string]struct{}, len(input))
	for _, logical := range input {
		output[logical.ProviderID] = struct{}{}
	}
	return output
}

func cloneProcessSpecs(input []ProcessSpec) []ProcessSpec {
	output := append([]ProcessSpec(nil), input...)
	for index := range output {
		output[index] = cloneProcessSpec(output[index])
	}
	return output
}

func cloneProcessSpec(input ProcessSpec) ProcessSpec {
	input.Args = append([]string(nil), input.Args...)
	input.Env = append([]string(nil), input.Env...)
	input.Providers = cloneLogicalProviders(input.Providers)
	return input
}

func cloneLogicalProviders(input []LogicalProviderSpec) []LogicalProviderSpec {
	return append([]LogicalProviderSpec(nil), input...)
}

func cloneLeases(input []readiness.ProviderSnapshot) []readiness.ProviderSnapshot {
	output := append([]readiness.ProviderSnapshot(nil), input...)
	for index := range output {
		output[index].Capabilities = append([]readiness.CapabilityKind(nil), output[index].Capabilities...)
	}
	return output
}

func validID(input string) bool {
	if len(input) == 0 || len(input) > maxSupervisorIDBytes || input != strings.TrimSpace(input) {
		return false
	}
	for index := 0; index < len(input); index++ {
		value := input[index]
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '-' || value == '_' || value == '.' {
			continue
		}
		return false
	}
	return true
}

func randomInstanceID(processID string) (string, error) {
	const op = "create local worker instance id"
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fault.New(fault.Unavailable, op, err)
	}
	return processID + "-" + hex.EncodeToString(buffer), nil
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
