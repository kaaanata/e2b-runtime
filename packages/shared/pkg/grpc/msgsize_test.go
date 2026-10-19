package grpc

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// defaultGRPCMessageSize is grpc-go's default receive limit, which a template
// build with inlined files used to exceed.
const defaultGRPCMessageSize = 4 << 20

type templateService struct {
	templatemanager.UnimplementedTemplateServiceServer

	received chan int
}

func (s templateService) TemplateCreate(_ context.Context, req *templatemanager.TemplateCreateRequest) (*emptypb.Empty, error) {
	size := 0
	for _, step := range req.GetTemplate().GetSteps() {
		for _, arg := range step.GetArgs() {
			size += len(arg)
		}
	}
	s.received <- size

	return &emptypb.Empty{}, nil
}

// buildRequest is a TemplateCreate whose steps carry payload bytes of base64-like
// text in RUN steps of 96 KiB, the way a client inlines a COPY source.
func buildRequest(payload int, marker string) *templatemanager.TemplateCreateRequest {
	const chunk = 96 << 10
	var steps []*templatemanager.TemplateStep
	for left := payload; left > 0; left -= chunk {
		steps = append(steps, &templatemanager.TemplateStep{
			Type: "RUN",
			Args: []string{"printf %s " + marker + strings.Repeat("A", min(chunk, left)-len(marker)) + " > /tmp/part"},
		})
	}

	return &templatemanager.TemplateCreateRequest{Template: &templatemanager.TemplateConfig{
		TemplateID: "tpl", BuildID: "build", Steps: steps,
	}}
}

func serveTemplateService(t *testing.T, received chan int, dialOpts ...grpc.DialOption) templatemanager.TemplateServiceClient {
	t.Helper()

	server := NewGRPCServer(&telemetry.Client{
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  metricnoop.NewMeterProvider(),
	})
	templatemanager.RegisterTemplateServiceServer(server, templateService{received: received})

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(listener.Addr().String(),
		append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOpts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return templatemanager.NewTemplateServiceClient(conn)
}

//nolint:paralleltest // the middleware logs through the global logger, which this test replaces
func TestNewGRPCServerAcceptsTemplateBuildAboveDefaultLimit(t *testing.T) {
	logs := captureLogs(t)
	received := make(chan int, 1)
	client := serveTemplateService(t, received, MessageSizeDialOption())

	payload := 3 * defaultGRPCMessageSize // 12 MiB of steps, well past the old 4 MiB limit
	_, err := client.TemplateCreate(t.Context(), buildRequest(payload, panicSentinel))

	require.NoError(t, err)
	require.Greater(t, <-received, defaultGRPCMessageSize)
	// The steps are user files: GRPCLogger drops payloads, keep it that way.
	require.NotContains(t, flattenLogs(logs), panicSentinel)
}

//nolint:paralleltest // the middleware logs through the global logger, which this test replaces
func TestNewGRPCServerRefusesMessagesAboveMaxMessageSize(t *testing.T) {
	captureLogs(t)
	received := make(chan int, 1)
	// grpc-go clients send without a limit by default: the server's bound decides.
	client := serveTemplateService(t, received)

	_, err := client.TemplateCreate(t.Context(), buildRequest(MaxMessageSize+1, "x"))

	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Empty(t, received)
}

func TestMessageSizeExceedsGRPCDefault(t *testing.T) {
	t.Parallel()

	require.Greater(t, MaxMessageSize, defaultGRPCMessageSize)
	require.Len(t, MessageSizeServerOptions(), 2)
}
