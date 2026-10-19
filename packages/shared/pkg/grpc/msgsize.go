package grpc

import "google.golang.org/grpc"

// MaxMessageSize is the largest gRPC message E2B's servers accept and its
// clients send or accept (64 MiB).
//
// grpc-go defaults to 4 MiB on receive. A template build request carries its
// build steps inline: a client that has no reachable upload URL for COPY
// sources embeds the files as RUN steps (a statically linked helper binary,
// compressed and base64-encoded, is several MiB), so the TemplateCreate
// request from the API to the template-manager can exceed the default and
// the build fails with ResourceExhausted before it starts. The bound still
// limits the memory a single request can pin; servers and clients share the
// constant so a message one side may send, the other side accepts.
const MaxMessageSize = 64 << 20

// MessageSizeServerOptions sets MaxMessageSize for both directions of a server.
func MessageSizeServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MaxMessageSize),
		grpc.MaxSendMsgSize(MaxMessageSize),
	}
}

// MessageSizeDialOption sets MaxMessageSize for both directions of every call
// on a client connection.
func MessageSizeDialOption() grpc.DialOption {
	return grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(MaxMessageSize),
		grpc.MaxCallSendMsgSize(MaxMessageSize),
	)
}
