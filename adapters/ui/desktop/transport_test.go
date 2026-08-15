package desktop

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/domain/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestGeneratedClientRejectInteractionUsesControlSubmitter(t *testing.T) {
	dependencies := newServerDependencies()
	desktopServer := newTestServer(t, dependencies)
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	platformv1.RegisterDesktopControlServiceServer(grpcServer, desktopServer)
	serveDone := make(chan error, 1)
	go func() { serveDone <- grpcServer.Serve(listener) }()

	var stopOnce sync.Once
	stopAndJoin := func() {
		stopOnce.Do(func() {
			grpcServer.Stop()
			_ = listener.Close()
			select {
			case err := <-serveDone:
				if err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
					t.Errorf("grpc Serve() error = %v", err)
				}
			case <-time.After(time.Second):
				t.Error("grpc server did not stop")
			}
		})
	}
	t.Cleanup(stopAndJoin)

	connection, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	rpcCtx, cancelRPC := context.WithTimeout(context.Background(), time.Second)
	defer cancelRPC()
	response, err := platformv1.NewDesktopControlServiceClient(connection).RejectInteraction(rpcCtx, &platformv1.RejectInteractionRequest{
		ProtocolVersion: protocolVersion,
		RequestId:       "wire-reject",
		SubjectId:       "user-1",
		TraceId:         "wire-trace",
	})
	if err != nil {
		t.Fatalf("RejectInteraction() error = %v", err)
	}
	if !response.GetAccepted() {
		t.Fatal("RejectInteraction().Accepted = false, want true")
	}
	commands := dependencies.controls.snapshot()
	if len(commands) != 1 || commands[0].Kind != control.StopAll || commands[0].Reason != control.ReasonUserRejected {
		t.Fatalf("control commands = %#v, want one USER_REJECTED StopAll", commands)
	}
	stopAndJoin()
}
