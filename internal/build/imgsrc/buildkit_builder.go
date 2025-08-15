package imgsrc

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/containerd/containerd/api/services/content/v1"
	"github.com/moby/buildkit/client"
	"github.com/superfly/flyctl/agent"
	"github.com/superfly/flyctl/helpers"
	"github.com/superfly/flyctl/internal/cmdfmt"
	"github.com/superfly/flyctl/internal/flyutil"
	"github.com/superfly/flyctl/internal/tracing"
	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/terminal"
	"go.opentelemetry.io/otel/trace"
)

var _ imageBuilder = (*BuildkitBuilder)(nil)

type BuildkitBuilder struct {
	addr string
}

func NewBuildkitBuilder(addr string) *BuildkitBuilder {
	return &BuildkitBuilder{addr: addr}
}

func (r *BuildkitBuilder) Name() string { return "Buildkit" }

func (r *BuildkitBuilder) Run(ctx context.Context, _ *dockerClientFactory, streams *iostreams.IOStreams, opts ImageOptions, build *build) (*DeploymentImage, string, error) {
	ctx, span := tracing.GetTracer().Start(ctx, "buildkit_builder", trace.WithAttributes(opts.ToSpanAttributes()...))
	defer span.End()

	build.BuildStart()
	defer build.BuildFinish()

	var dockerfile string

	switch {
	case opts.DockerfilePath != "" && !helpers.FileExists(opts.DockerfilePath):
		return nil, "", fmt.Errorf("dockerfile '%s' not found", opts.DockerfilePath)
	case opts.DockerfilePath != "":
		dockerfile = opts.DockerfilePath
	default:
		dockerfile = ResolveDockerfile(opts.WorkingDir)
	}

	if dockerfile == "" {
		terminal.Debug("dockerfile not found, skipping")
		return nil, "", nil
	}

	build.ImageBuildStart()
	defer build.ImageBuildFinish()

	image, err := r.buildWithBuildkit(ctx, streams, opts, dockerfile, build)
	if err != nil {
		return nil, "", err
	}
	cmdfmt.PrintDone(streams.ErrOut, "Building image done")
	span.SetAttributes(image.ToSpanAttributes()...)
	return image, "", nil
}

func (r *BuildkitBuilder) buildWithBuildkit(ctx context.Context, streams *iostreams.IOStreams, opts ImageOptions, dockerfilePath string, buildState *build) (i *DeploymentImage, err error) {
	ctx, span := tracing.GetTracer().Start(ctx, "buildkit_build", trace.WithAttributes(opts.ToSpanAttributes()...))
	defer func() {
		if err != nil {
			span.RecordError(err)
		}
		streams.StopProgressIndicator()
		span.End()
	}()

	buildState.BuilderInitStart()
	defer buildState.BuilderInitFinish()
	buildState.SetBuilderMetaPart1("buildkit", r.addr, "")

	msg := fmt.Sprintf("Connecting to buildkit daemon at %s...\n", r.addr)
	if streams.IsInteractive() {
		streams.StartProgressIndicatorMsg(msg)
	} else {
		fmt.Fprintln(streams.ErrOut, msg)
	}

	buildkitClient, err := client.New(ctx, r.addr)
	if err != nil {
		return nil, fmt.Errorf("failed to create buildkit client: %w", err)
	}
	defer buildkitClient.Close()

	if _, err = buildkitClient.Info(ctx); err != nil {
		terminal.Debug("Direct connection failed, trying via wireguard...")
		apiClient := flyutil.ClientFromContext(ctx)
		app, err := apiClient.GetAppCompact(ctx, opts.AppName)
		if err != nil {
			return nil, fmt.Errorf("failed to get app info for %s: %w", opts.AppName, err)
		}
		_, dialer, err := agent.BringUpAgent(ctx, apiClient, app, app.Network, true)
		if err != nil {
			return nil, fmt.Errorf("failed wireguard connection: %w", err)
		}
		buildkitClient, err = client.New(ctx, r.addr, client.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to connect to buildkit daemon at %s via wireguard: %w", r.addr, err)
		}
		terminal.Debug("Successfully connected via wireguard")
	}

	streams.StopProgressIndicator()
	cmdfmt.PrintDone(streams.ErrOut, fmt.Sprintf("Connected to buildkit daemon at %s", r.addr))

	buildState.BuildAndPushStart()
	defer buildState.BuildAndPushFinish()

	res, err := buildImage(ctx, buildkitClient, opts, dockerfilePath)
	if err != nil {
		return nil, err
	}

	return newDeploymentImage(ctx, buildkitClient, res, opts.Tag)
}

func readContent(ctx context.Context, contentClient content.ContentClient, desc *Descriptor) (string, error) {
	readClient, err := contentClient.Read(ctx, &content.ReadContentRequest{Digest: desc.Digest})
	if err != nil {
		return "", fmt.Errorf("failed to create read stream: %w", err)
	}
	var data []byte
	for {
		resp, err := readClient.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("failed to read from stream: %w", err)
		}
		data = append(data, resp.Data...)
	}
	return string(data), nil
}
