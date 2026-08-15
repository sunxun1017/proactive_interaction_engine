package control

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGeneratedControlStreamingRoundTripAndBoundedShutdown(t *testing.T) {
	control, _, _, clock := newControlTestServer(t)
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	platformv1.RegisterIdentityWorkerControlServiceServer(grpcServer, control)
	serveDone := make(chan error, 1)
	go func() { serveDone <- grpcServer.Serve(listener) }()

	connection, err := grpc.NewClient(
		"passthrough:///identity-worker-control",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Errorf("close control client: %v", err)
		}
		grpcServer.Stop()
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close control listener: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("identity worker control transport did not stop")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := platformv1.NewIdentityWorkerControlServiceClient(connection).WatchVision(ctx, validVisionWatch())
	if err != nil {
		t.Fatalf("WatchVision() error = %v", err)
	}
	initial, err := stream.Recv()
	if err != nil || initial.GetRevision() != 1 || initial.GetIdle() == nil {
		t.Fatalf("initial Recv() = %#v, %v", initial, err)
	}
	if err := control.PublishVision(context.Background(), "vision-instance", VisionWork{
		EvidenceWindowID: "transport-window", OpenedAt: clock.Now(), Deadline: clock.Now().Add(time.Second), Detection: &FaceDetectionTask{},
	}); err != nil {
		t.Fatalf("PublishVision() error = %v", err)
	}
	active, err := stream.Recv()
	if err != nil || active.GetRevision() != 2 || active.GetActive().GetEvidenceWindowId() != "transport-window" {
		t.Fatalf("active Recv() = %#v, %v", active, err)
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("Recv() after cancel code = %s, want Canceled", status.Code(err))
	}
}
