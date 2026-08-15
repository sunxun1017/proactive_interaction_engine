package registry

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/application/readiness"
	engineclock "proactive-interaction-engine/internal/runtime/clock"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestGeneratedGRPCRegisterRoundTrip(t *testing.T) {
	clock := engineclock.NewFake(testNow())
	registry, err := New(clock, time.Minute)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	platformv1.RegisterCapabilityProviderRegistryServiceServer(server, registry)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	var connection *grpc.ClientConn
	t.Cleanup(func() {
		if connection != nil {
			if err := connection.Close(); err != nil {
				t.Errorf("close client connection: %v", err)
			}
		}
		server.Stop()
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve() did not stop")
		}
	})

	connection, err = grpc.NewClient(
		"passthrough:///capability-registry",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	client := platformv1.NewCapabilityProviderRegistryServiceClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := client.RegisterCapabilityProvider(ctx, validRegistration(
		"camera-main",
		"camera-instance",
		platformv1.ServiceCapabilityKind_SERVICE_CAPABILITY_KIND_PERSON_PRESENCE,
	))
	if err != nil {
		t.Fatalf("RegisterCapabilityProvider() error = %v", err)
	}
	if response.GetLeaseId() == "" || response.GetExpiresAt().AsTime() != testNow().Add(time.Minute) {
		t.Fatalf("wire response = %#v, want server lease expiring at %s", response, testNow().Add(time.Minute))
	}

	snapshots := registry.Snapshots()
	want := readiness.ProviderSnapshot{
		ProviderID:            "camera-main",
		InstanceID:            "camera-instance",
		ProtocolVersion:       SupportedProtocolVersion,
		ImplementationVersion: "test-v1",
		Capabilities:          []readiness.CapabilityKind{readiness.PersonPresence},
		Health:                readiness.Healthy,
		LeaseExpiresAt:        testNow().Add(time.Minute),
		OperationalProfile: readiness.ProviderOperationalProfile{
			PrivacyClass:          readiness.ProviderPrivacyDeviceLocal,
			MaximumLatency:        250 * time.Millisecond,
			CancellationSemantics: readiness.ProviderCancellationBounded,
			DeviceRequirements: []readiness.ProviderDeviceClass{
				readiness.ProviderDeviceCamera,
				readiness.ProviderDeviceMicrophone,
			},
		},
	}
	if len(snapshots) != 1 || !reflect.DeepEqual(snapshots[0], want) {
		t.Fatalf("Snapshots() = %#v, want %#v", snapshots, want)
	}
}
