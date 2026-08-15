package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/port"
	"proactive-interaction-engine/internal/application/readiness"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SupportedProtocolVersion is the one registry protocol accepted by this
// implementation.
const SupportedProtocolVersion = "v1"

const leaseIDBytes = 16

// Registry owns the current in-memory provider leases. It does not run a
// cleanup goroutine; expired snapshots remain visible until explicit
// replacement or unregistration.
type Registry struct {
	platformv1.UnimplementedCapabilityProviderRegistryServiceServer

	clock         port.Clock
	leaseDuration time.Duration

	mu         sync.RWMutex
	byProvider map[string]*providerRecord
	byLease    map[string]*providerRecord
	subs       map[uint64]chan []readiness.ProviderSnapshot
	nextSubID  uint64
}

type providerRecord struct {
	declaration registrationDeclaration
	leaseID     string
	snapshot    readiness.ProviderSnapshot
}

// New constructs an empty registry with server-owned lease timing.
func New(clock port.Clock, leaseDuration time.Duration) (*Registry, error) {
	if clock == nil {
		return nil, errors.New("capability registry clock is required")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("capability registry lease duration must be positive")
	}
	return &Registry{
		clock:         clock,
		leaseDuration: leaseDuration,
		byProvider:    make(map[string]*providerRecord),
		byLease:       make(map[string]*providerRecord),
		subs:          make(map[uint64]chan []readiness.ProviderSnapshot),
	}, nil
}

func (r *Registry) RegisterCapabilityProvider(ctx context.Context, request *platformv1.RegisterCapabilityProviderRequest) (*platformv1.RegisterCapabilityProviderResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	declaration, err := normalizeRegistration(request)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	now := r.clock.Now()
	if current, exists := r.byProvider[declaration.providerID]; exists && now.Before(current.snapshot.LeaseExpiresAt) {
		if !sameDeclaration(current.declaration, declaration) {
			return nil, status.Error(codes.AlreadyExists, "active provider id has a different declaration")
		}
		return registerResponse(current), nil
	}

	leaseID, err := r.newLeaseIDLocked()
	if err != nil {
		return nil, status.Error(codes.Internal, "issue capability provider lease")
	}
	expiresAt := now.Add(r.leaseDuration)
	record := &providerRecord{
		declaration: declaration,
		leaseID:     leaseID,
		snapshot: readiness.ProviderSnapshot{
			ProviderID:            declaration.providerID,
			InstanceID:            declaration.instanceID,
			ProtocolVersion:       declaration.protocolVersion,
			ImplementationVersion: declaration.implementationVersion,
			Capabilities:          cloneCapabilities(declaration.capabilities),
			OperationalProfile:    cloneOperationalProfile(declaration.operationalProfile),
			Health:                declaration.health,
			HealthReason:          declaration.healthReason,
			LeaseExpiresAt:        expiresAt,
		},
	}
	if previous, exists := r.byProvider[declaration.providerID]; exists {
		delete(r.byLease, previous.leaseID)
	}
	r.byProvider[declaration.providerID] = record
	r.byLease[leaseID] = record
	r.notifySubscribersLocked()
	return registerResponse(record), nil
}

func (r *Registry) HeartbeatCapabilityProvider(ctx context.Context, request *platformv1.HeartbeatCapabilityProviderRequest) (*platformv1.HeartbeatCapabilityProviderResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	leaseID, health, healthReason, err := normalizeHeartbeat(request)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	record, exists := r.byLease[leaseID]
	if !exists {
		return nil, status.Error(codes.NotFound, "capability provider lease not found")
	}
	now := r.clock.Now()
	if !now.Before(record.snapshot.LeaseExpiresAt) {
		return nil, status.Error(codes.DeadlineExceeded, "capability provider lease expired")
	}
	record.snapshot.Health = health
	record.snapshot.HealthReason = healthReason
	record.snapshot.LeaseExpiresAt = now.Add(r.leaseDuration)
	r.notifySubscribersLocked()
	return &platformv1.HeartbeatCapabilityProviderResponse{
		ExpiresAt: timestamppb.New(record.snapshot.LeaseExpiresAt),
	}, nil
}

func (r *Registry) UnregisterCapabilityProvider(ctx context.Context, request *platformv1.UnregisterCapabilityProviderRequest) (*platformv1.UnregisterCapabilityProviderResponse, error) {
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.GetLeaseId() == "" {
		return nil, status.Error(codes.InvalidArgument, "lease id is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := contextStatus(ctx); err != nil {
		return nil, err
	}
	record, exists := r.byLease[request.GetLeaseId()]
	if !exists {
		return nil, status.Error(codes.NotFound, "capability provider lease not found")
	}
	delete(r.byLease, record.leaseID)
	if current, exists := r.byProvider[record.declaration.providerID]; exists && current == record {
		delete(r.byProvider, record.declaration.providerID)
	}
	r.notifySubscribersLocked()
	return &platformv1.UnregisterCapabilityProviderResponse{}, nil
}

// Snapshots returns all current records, including expired records, sorted by
// provider ID. Returned values and their capability slices are independent.
func (r *Registry) Snapshots() []readiness.ProviderSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotsLocked()
}

// Subscribe returns an immutable current snapshot and a latest-only stream of
// successful registry mutations. Lease expiry remains a reader-side time
// decision and does not create a synthetic mutation.
func (r *Registry) Subscribe() ([]readiness.ProviderSnapshot, <-chan []readiness.ProviderSnapshot, func()) {
	r.mu.Lock()
	id := r.nextSubID
	r.nextSubID++
	updates := make(chan []readiness.ProviderSnapshot, 1)
	r.subs[id] = updates
	initial := r.snapshotsLocked()
	r.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			r.mu.Lock()
			if existing, ok := r.subs[id]; ok {
				delete(r.subs, id)
				close(existing)
			}
			r.mu.Unlock()
		})
	}
	return initial, updates, cancel
}

func (r *Registry) snapshotsLocked() []readiness.ProviderSnapshot {

	snapshots := make([]readiness.ProviderSnapshot, 0, len(r.byProvider))
	for _, record := range r.byProvider {
		snapshot := record.snapshot
		snapshot.Capabilities = cloneCapabilities(record.snapshot.Capabilities)
		snapshot.OperationalProfile = cloneOperationalProfile(record.snapshot.OperationalProfile)
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ProviderID < snapshots[j].ProviderID
	})
	return snapshots
}

func (r *Registry) notifySubscribersLocked() {
	for _, subscriber := range r.subs {
		snapshots := r.snapshotsLocked()
		select {
		case subscriber <- snapshots:
		default:
			select {
			case <-subscriber:
			default:
			}
			subscriber <- snapshots
		}
	}
}

// LeaseSnapshot returns the record currently bound to leaseID without
// interpreting its expiry. Callers apply their own injected clock at use time.
func (r *Registry) LeaseSnapshot(leaseID string) (readiness.ProviderSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	record, exists := r.byLease[leaseID]
	if !exists {
		return readiness.ProviderSnapshot{}, false
	}
	snapshot := record.snapshot
	snapshot.Capabilities = cloneCapabilities(record.snapshot.Capabilities)
	snapshot.OperationalProfile = cloneOperationalProfile(record.snapshot.OperationalProfile)
	return snapshot, true
}

// RevokeProvider removes the current local lease for providerID. It is used by
// the composition-owned process supervisor when a child can no longer
// unregister itself, preventing a dead instance from appearing healthy.
func (r *Registry) RevokeProvider(providerID string) bool {
	if providerID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.byProvider[providerID]
	if !exists {
		return false
	}
	delete(r.byProvider, providerID)
	delete(r.byLease, record.leaseID)
	r.notifySubscribersLocked()
	return true
}

func sameDeclaration(left, right registrationDeclaration) bool {
	if left.providerID != right.providerID ||
		left.instanceID != right.instanceID ||
		left.protocolVersion != right.protocolVersion ||
		left.implementationVersion != right.implementationVersion ||
		left.health != right.health ||
		left.healthReason != right.healthReason ||
		!sameOperationalProfile(left.operationalProfile, right.operationalProfile) ||
		len(left.capabilities) != len(right.capabilities) {
		return false
	}
	for index := range left.capabilities {
		if left.capabilities[index] != right.capabilities[index] {
			return false
		}
	}
	return true
}

func registerResponse(record *providerRecord) *platformv1.RegisterCapabilityProviderResponse {
	return &platformv1.RegisterCapabilityProviderResponse{
		LeaseId:   record.leaseID,
		ExpiresAt: timestamppb.New(record.snapshot.LeaseExpiresAt),
	}
}

func (r *Registry) newLeaseIDLocked() (string, error) {
	for {
		bytes := make([]byte, leaseIDBytes)
		if _, err := rand.Read(bytes); err != nil {
			return "", err
		}
		leaseID := hex.EncodeToString(bytes)
		if _, exists := r.byLease[leaseID]; !exists {
			return leaseID, nil
		}
	}
}

func cloneCapabilities(input []readiness.CapabilityKind) []readiness.CapabilityKind {
	return append([]readiness.CapabilityKind(nil), input...)
}

func cloneOperationalProfile(input readiness.ProviderOperationalProfile) readiness.ProviderOperationalProfile {
	input.DeviceRequirements = append([]readiness.ProviderDeviceClass(nil), input.DeviceRequirements...)
	return input
}

func sameOperationalProfile(left, right readiness.ProviderOperationalProfile) bool {
	if left.PrivacyClass != right.PrivacyClass ||
		left.MaximumLatency != right.MaximumLatency ||
		left.CancellationSemantics != right.CancellationSemantics ||
		len(left.DeviceRequirements) != len(right.DeviceRequirements) {
		return false
	}
	for index := range left.DeviceRequirements {
		if left.DeviceRequirements[index] != right.DeviceRequirements[index] {
			return false
		}
	}
	return true
}

func contextStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}
