package dockerx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"caddytower/internal/config"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestListContainersByLabelSortsAndTrimsName(t *testing.T) {
	t.Parallel()

	fake := &fakeAPIClient{
		containerListFn: func(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
			if got := opts.Filters.Get("label"); len(got) != 1 || got[0] != "app=demo" {
				t.Fatalf("expected label filter, got %#v", got)
			}

			return []types.Container{
				{ID: "2", Names: []string{"/zeta"}, Image: "img2", Labels: map[string]string{"app": "demo"}},
				{ID: "1", Names: []string{"/alpha"}, Image: "img1", Labels: map[string]string{"app": "demo"}},
			}, nil
		},
	}

	service := New(fake)
	items, err := service.ListContainersByLabel(context.Background(), "app", "demo")
	if err != nil {
		t.Fatalf("ListContainersByLabel() error = %v", err)
	}

	if items[0].Name != "alpha" || items[1].Name != "zeta" {
		t.Fatalf("unexpected order: %#v", items)
	}
}

func TestRecreateContainerCreatesStartsAndInspects(t *testing.T) {
	t.Parallel()

	fake := &fakeAPIClient{
		networkListFn: func(context.Context, network.ListOptions) ([]network.Summary, error) {
			return nil, nil
		},
		networkCreateFn: func(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
			if name != "edge" {
				t.Fatalf("network name = %q", name)
			}
			return network.CreateResponse{ID: "edge-id"}, nil
		},
		containerInspectFn: func(_ context.Context, name string) (types.ContainerJSON, error) {
			switch name {
			case "demo":
				return types.ContainerJSON{}, errdefs.NotFound(errors.New("missing"))
			case "created-1":
				return types.ContainerJSON{
					ContainerJSONBase: &types.ContainerJSONBase{
						ID:    "created-1",
						Name:  "/demo",
						State: &types.ContainerState{Running: true},
					},
					Config: &container.Config{
						Image:  "ghcr.io/example/demo:latest",
						Labels: map[string]string{"app": "demo"},
					},
					NetworkSettings: &types.NetworkSettings{
						Networks: map[string]*network.EndpointSettings{"edge": {}},
					},
				}, nil
			default:
				t.Fatalf("unexpected inspect target: %q", name)
				return types.ContainerJSON{}, nil
			}
		},
		containerCreateFn: func(_ context.Context, cfg *container.Config, host *container.HostConfig, netCfg *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
			if name != "demo" {
				t.Fatalf("container name = %q", name)
			}
			if cfg.Image != "ghcr.io/example/demo:latest" {
				t.Fatalf("image = %q", cfg.Image)
			}
			if got := cfg.Env[0]; got != "APP_ENV=prod" {
				t.Fatalf("env = %#v", cfg.Env)
			}
			if _, ok := cfg.ExposedPorts[nat.Port("3000/tcp")]; !ok {
				t.Fatalf("missing exposed port: %#v", cfg.ExposedPorts)
			}
			if got := host.RestartPolicy.Name; got != "unless-stopped" {
				t.Fatalf("restart policy = %q", got)
			}
			if got := host.PortBindings[nat.Port("3000/tcp")][0].HostPort; got != "8080" {
				t.Fatalf("host port = %q", got)
			}
			if _, ok := netCfg.EndpointsConfig["edge"]; !ok {
				t.Fatalf("missing edge network endpoint: %#v", netCfg.EndpointsConfig)
			}
			return container.CreateResponse{ID: "created-1"}, nil
		},
		containerStartFn: func(_ context.Context, id string, _ container.StartOptions) error {
			if id != "created-1" {
				t.Fatalf("started id = %q", id)
			}
			return nil
		},
	}

	service := New(fake)
	inspect, err := service.RecreateContainer(context.Background(), ContainerSpec{
		Name:    "demo",
		Image:   "ghcr.io/example/demo:latest",
		Network: "edge",
		Env: map[string]string{
			"APP_ENV": "prod",
		},
		PublishedPorts: []PortBinding{{
			ContainerPort: "3000",
			HostPort:      "8080",
		}},
	})
	if err != nil {
		t.Fatalf("RecreateContainer() error = %v", err)
	}

	if inspect.Name != "demo" || !ContainsNetwork(inspect, "edge") {
		t.Fatalf("inspect = %#v", inspect)
	}
}

func TestPullImageDrainsReader(t *testing.T) {
	t.Parallel()

	fake := &fakeAPIClient{
		imagePullFn: func(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("{\"status\":\"Pulling from knopkem/cameos\"}\n{\"status\":\"done\"}\n")), nil
		},
	}

	service := New(fake)
	if err := service.PullImage(context.Background(), "ghcr.io/example/demo:latest"); err != nil {
		t.Fatalf("PullImage() error = %v", err)
	}
}

func TestPullImageReturnsStreamError(t *testing.T) {
	t.Parallel()

	fake := &fakeAPIClient{
		imagePullFn: func(context.Context, string, image.PullOptions) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("{\"errorDetail\":{\"message\":\"denied\"},\"error\":\"denied\"}\n")), nil
		},
	}

	service := New(fake)
	err := service.PullImage(context.Background(), "ghcr.io/knopkem/cameos:latest")
	if err == nil {
		t.Fatal("expected pull error")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecStreamsOutputAndChecksExitCode(t *testing.T) {
	t.Parallel()

	fake := &fakeAPIClient{
		containerExecCreateFn: func(_ context.Context, containerName string, opts container.ExecOptions) (types.IDResponse, error) {
			if containerName != "demo" {
				t.Fatalf("containerName = %q", containerName)
			}
			if got := strings.Join(opts.Cmd, " "); got != "echo hello" {
				t.Fatalf("cmd = %q", got)
			}
			return types.IDResponse{ID: "exec-1"}, nil
		},
		containerExecAttachFn: func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
			server, client := net.Pipe()
			go func() {
				defer server.Close()
				writer := stdcopy.NewStdWriter(server, stdcopy.Stdout)
				_, _ = io.WriteString(writer, "hello from exec\n")
			}()
			return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
		},
		containerExecInspectFn: func(context.Context, string) (container.ExecInspect, error) {
			return container.ExecInspect{Running: false, ExitCode: 0}, nil
		},
	}

	service := New(fake)
	var stdout strings.Builder
	if err := service.Exec(context.Background(), "demo", []string{"echo", "hello"}, nil, &stdout, io.Discard); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if got := stdout.String(); got != "hello from exec\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestNewFromEnvUsesExplicitDockerHost(t *testing.T) {
	t.Parallel()

	_, err := NewFromEnv(config.Config{
		DockerHost: "tcp://127.0.0.1:65535",
	})
	if err != nil && !strings.Contains(err.Error(), "create docker client") {
		t.Fatalf("unexpected error = %v", err)
	}
}

type fakeAPIClient struct {
	pingFn                 func(context.Context) (types.Ping, error)
	imagePullFn            func(context.Context, string, image.PullOptions) (io.ReadCloser, error)
	containerListFn        func(context.Context, container.ListOptions) ([]types.Container, error)
	containerInspectFn     func(context.Context, string) (types.ContainerJSON, error)
	containerStopFn        func(context.Context, string, container.StopOptions) error
	containerRemoveFn      func(context.Context, string, container.RemoveOptions) error
	containerCreateFn      func(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	containerStartFn       func(context.Context, string, container.StartOptions) error
	containerLogsFn        func(context.Context, string, container.LogsOptions) (io.ReadCloser, error)
	containerExecCreateFn  func(context.Context, string, container.ExecOptions) (types.IDResponse, error)
	containerExecAttachFn  func(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error)
	containerExecInspectFn func(context.Context, string) (container.ExecInspect, error)
	networkListFn          func(context.Context, network.ListOptions) ([]network.Summary, error)
	networkCreateFn        func(context.Context, string, network.CreateOptions) (network.CreateResponse, error)
	networkConnectFn       func(context.Context, string, string, *network.EndpointSettings) error
}

func (f *fakeAPIClient) Ping(ctx context.Context) (types.Ping, error) {
	if f.pingFn == nil {
		return types.Ping{}, nil
	}
	return f.pingFn(ctx)
}

func (f *fakeAPIClient) ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	return f.imagePullFn(ctx, ref, opts)
}

func (f *fakeAPIClient) ContainerList(ctx context.Context, opts container.ListOptions) ([]types.Container, error) {
	return f.containerListFn(ctx, opts)
}

func (f *fakeAPIClient) ContainerInspect(ctx context.Context, name string) (types.ContainerJSON, error) {
	return f.containerInspectFn(ctx, name)
}

func (f *fakeAPIClient) ContainerStop(ctx context.Context, name string, opts container.StopOptions) error {
	if f.containerStopFn == nil {
		return nil
	}
	return f.containerStopFn(ctx, name, opts)
}

func (f *fakeAPIClient) ContainerRemove(ctx context.Context, name string, opts container.RemoveOptions) error {
	if f.containerRemoveFn == nil {
		return nil
	}
	return f.containerRemoveFn(ctx, name, opts)
}

func (f *fakeAPIClient) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, netCfg *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	return f.containerCreateFn(ctx, cfg, host, netCfg, platform, name)
}

func (f *fakeAPIClient) ContainerStart(ctx context.Context, id string, opts container.StartOptions) error {
	return f.containerStartFn(ctx, id, opts)
}

func (f *fakeAPIClient) ContainerLogs(ctx context.Context, name string, opts container.LogsOptions) (io.ReadCloser, error) {
	return f.containerLogsFn(ctx, name, opts)
}

func (f *fakeAPIClient) ContainerExecCreate(ctx context.Context, containerName string, opts container.ExecOptions) (types.IDResponse, error) {
	if f.containerExecCreateFn == nil {
		return types.IDResponse{ID: "exec-default"}, nil
	}
	return f.containerExecCreateFn(ctx, containerName, opts)
}

func (f *fakeAPIClient) ContainerExecAttach(ctx context.Context, execID string, opts container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.containerExecAttachFn == nil {
		server, client := net.Pipe()
		go server.Close()
		return types.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	return f.containerExecAttachFn(ctx, execID, opts)
}

func (f *fakeAPIClient) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	if f.containerExecInspectFn == nil {
		return container.ExecInspect{ExecID: execID, ExitCode: 0}, nil
	}
	return f.containerExecInspectFn(ctx, execID)
}

func (f *fakeAPIClient) NetworkList(ctx context.Context, opts network.ListOptions) ([]network.Summary, error) {
	return f.networkListFn(ctx, opts)
}

func (f *fakeAPIClient) NetworkCreate(ctx context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	return f.networkCreateFn(ctx, name, opts)
}

func (f *fakeAPIClient) NetworkConnect(ctx context.Context, networkID, containerName string, endpoint *network.EndpointSettings) error {
	if f.networkConnectFn == nil {
		return nil
	}
	return f.networkConnectFn(ctx, networkID, containerName, endpoint)
}
