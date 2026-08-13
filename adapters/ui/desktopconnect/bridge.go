package desktopconnect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"

	"connectrpc.com/connect"
	"proactive-interaction-engine/adapters/ui/desktop"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/internal/domain/fault"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Bridge adapts generated Connect handlers to the single desktop service.
type Bridge struct {
	service *desktop.Server
}

// New constructs a transport bridge for an existing desktop service.
func New(service *desktop.Server) (*Bridge, error) {
	if service == nil {
		return nil, fault.New(fault.InvalidInput, "create desktop connect bridge", errors.New("desktop service is required"))
	}
	return &Bridge{service: service}, nil
}

func (b *Bridge) GetState(ctx context.Context, request *connect.Request[platformv1.GetStateRequest]) (*connect.Response[platformv1.GetStateResponse], error) {
	if request == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request is required"))
	}
	response, err := b.service.GetState(enrichedContext(ctx, request.Header()), request.Msg)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(response), nil
}

func (b *Bridge) SetPermission(ctx context.Context, request *connect.Request[platformv1.SetPermissionRequest]) (*connect.Response[platformv1.SetPermissionResponse], error) {
	if request == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request is required"))
	}
	response, err := b.service.SetPermission(enrichedContext(ctx, request.Header()), request.Msg)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(response), nil
}

func (b *Bridge) RejectInteraction(ctx context.Context, request *connect.Request[platformv1.RejectInteractionRequest]) (*connect.Response[platformv1.RejectInteractionResponse], error) {
	if request == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request is required"))
	}
	response, err := b.service.RejectInteraction(enrichedContext(ctx, request.Header()), request.Msg)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(response), nil
}

func (b *Bridge) WatchState(
	ctx context.Context,
	request *connect.Request[platformv1.WatchStateRequest],
	stream *connect.ServerStream[platformv1.WatchStateResponse],
) error {
	if request == nil || stream == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("request and stream are required"))
	}
	adapter := &watchStream{
		ctx:    enrichedContext(ctx, request.Header()),
		stream: stream,
	}
	if err := b.service.WatchState(request.Msg, adapter); err != nil {
		return connectError(err)
	}
	return nil
}

func enrichedContext(ctx context.Context, header http.Header) context.Context {
	values := metadata.MD{}
	if authorization := header.Values("authorization"); len(authorization) != 0 {
		values["authorization"] = append([]string(nil), authorization...)
	}
	if origin := header.Values("origin"); len(origin) != 0 {
		values["origin"] = append([]string(nil), origin...)
	}
	return metadata.NewIncomingContext(ctx, values)
}

func connectError(err error) error {
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return connect.NewError(connect.CodeInternal, errors.New("desktop service failed"))
	}
	return connect.NewError(connectCode(grpcStatus.Code()), errors.New(grpcStatus.Message()))
}

func connectCode(code codes.Code) connect.Code {
	switch code {
	case codes.Canceled:
		return connect.CodeCanceled
	case codes.Unknown:
		return connect.CodeUnknown
	case codes.InvalidArgument:
		return connect.CodeInvalidArgument
	case codes.DeadlineExceeded:
		return connect.CodeDeadlineExceeded
	case codes.NotFound:
		return connect.CodeNotFound
	case codes.AlreadyExists:
		return connect.CodeAlreadyExists
	case codes.PermissionDenied:
		return connect.CodePermissionDenied
	case codes.ResourceExhausted:
		return connect.CodeResourceExhausted
	case codes.FailedPrecondition:
		return connect.CodeFailedPrecondition
	case codes.Aborted:
		return connect.CodeAborted
	case codes.OutOfRange:
		return connect.CodeOutOfRange
	case codes.Unimplemented:
		return connect.CodeUnimplemented
	case codes.Internal:
		return connect.CodeInternal
	case codes.Unavailable:
		return connect.CodeUnavailable
	case codes.DataLoss:
		return connect.CodeDataLoss
	case codes.Unauthenticated:
		return connect.CodeUnauthenticated
	default:
		return connect.CodeUnknown
	}
}

// PeerContext injects only a concrete TCP peer parsed from RemoteAddr. It does
// not trust Host, X-Forwarded-For, Forwarded, or any other proxy assertion.
func PeerContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ctx := request.Context()
		host, port, err := net.SplitHostPort(request.RemoteAddr)
		if err == nil {
			ip := net.ParseIP(host)
			parsedPort, portErr := strconv.Atoi(port)
			if ip != nil && portErr == nil && parsedPort > 0 && parsedPort <= 65535 {
				ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{IP: ip, Port: parsedPort}})
			}
		}
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

type watchStream struct {
	ctx    context.Context
	stream *connect.ServerStream[platformv1.WatchStateResponse]
}

func (s *watchStream) Send(response *platformv1.WatchStateResponse) error {
	return s.stream.Send(response)
}

func (s *watchStream) SetHeader(metadata.MD) error { return nil }

func (s *watchStream) SendHeader(metadata.MD) error { return nil }

func (s *watchStream) SetTrailer(metadata.MD) {}

func (s *watchStream) Context() context.Context { return s.ctx }

func (s *watchStream) SendMsg(message any) error {
	response, ok := message.(*platformv1.WatchStateResponse)
	if !ok {
		return errors.New("invalid desktop watch response")
	}
	return s.Send(response)
}

func (*watchStream) RecvMsg(any) error { return errors.New("desktop watch is server streaming") }
