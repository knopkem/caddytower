package dockerx

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"caddytower/internal/config"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type apiClient interface {
	Ping(context.Context) (types.Ping, error)
	ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error)
	ContainerList(context.Context, container.ListOptions) ([]types.Container, error)
	ContainerInspect(context.Context, string) (types.ContainerJSON, error)
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error)
	NetworkList(context.Context, network.ListOptions) ([]network.Summary, error)
	NetworkCreate(context.Context, string, network.CreateOptions) (network.CreateResponse, error)
	NetworkConnect(context.Context, string, string, *network.EndpointSettings) error
}

type Service struct {
	api apiClient
}

type PortBinding struct {
	ContainerPort string
	HostPort      string
	HostIP        string
	Protocol      string
}

type Mount struct {
	Source string
	Target string
}

type ContainerSpec struct {
	Name           string
	Image          string
	Env            map[string]string
	Labels         map[string]string
	Network        string
	Mounts         []Mount
	ExposedPorts   []string
	PublishedPorts []PortBinding
	RestartPolicy  string
}

type ContainerSummary struct {
	ID     string
	Name   string
	Image  string
	State  string
	Status string
	Labels map[string]string
}

type ContainerInspect struct {
	ID       string
	Name     string
	Image    string
	Running  bool
	Networks []string
	Labels   map[string]string
}

func New(api apiClient) *Service {
	return &Service{api: api}
}

func NewFromEnv(cfg config.Config) (*Service, error) {
	opts := []client.Opt{
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	}
	if strings.TrimSpace(cfg.DockerHost) != "" {
		opts = append(opts, client.WithHost(cfg.DockerHost))
	}

	api, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	return New(api), nil
}

func (s *Service) Ping(ctx context.Context) error {
	if _, err := s.api.Ping(ctx); err != nil {
		return fmt.Errorf("ping docker daemon: %w", err)
	}
	return nil
}

func (s *Service) PullImage(ctx context.Context, imageRef string) error {
	reader, err := s.api.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", imageRef, err)
	}
	defer reader.Close()

	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("drain image pull stream for %s: %w", imageRef, err)
	}

	return nil
}

func (s *Service) ListContainersByLabel(ctx context.Context, key, value string) ([]ContainerSummary, error) {
	args := filters.NewArgs()
	if value == "" {
		args.Add("label", key)
	} else {
		args.Add("label", key+"="+value)
	}

	containers, err := s.api.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers by label %s: %w", key, err)
	}

	summaries := make([]ContainerSummary, 0, len(containers))
	for _, item := range containers {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}

		summaries = append(summaries, ContainerSummary{
			ID:     item.ID,
			Name:   name,
			Image:  item.Image,
			State:  item.State,
			Status: item.Status,
			Labels: mapsClone(item.Labels),
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})

	return summaries, nil
}

func (s *Service) InspectContainer(ctx context.Context, containerName string) (ContainerInspect, error) {
	item, err := s.api.ContainerInspect(ctx, containerName)
	if err != nil {
		return ContainerInspect{}, fmt.Errorf("inspect container %s: %w", containerName, err)
	}

	networks := make([]string, 0, len(item.NetworkSettings.Networks))
	for networkName := range item.NetworkSettings.Networks {
		networks = append(networks, networkName)
	}
	sort.Strings(networks)

	return ContainerInspect{
		ID:       item.ID,
		Name:     strings.TrimPrefix(item.Name, "/"),
		Image:    item.Config.Image,
		Running:  item.ContainerJSONBase.State.Running,
		Networks: networks,
		Labels:   mapsClone(item.Config.Labels),
	}, nil
}

func (s *Service) EnsureNetwork(ctx context.Context, networkName string) error {
	args := filters.NewArgs()
	args.Add("name", networkName)

	networks, err := s.api.NetworkList(ctx, network.ListOptions{Filters: args})
	if err != nil {
		return fmt.Errorf("list network %s: %w", networkName, err)
	}

	for _, item := range networks {
		if item.Name == networkName {
			return nil
		}
	}

	if _, err := s.api.NetworkCreate(ctx, networkName, network.CreateOptions{}); err != nil {
		return fmt.Errorf("create network %s: %w", networkName, err)
	}

	return nil
}

func (s *Service) EnsureContainerOnNetwork(ctx context.Context, networkName, containerName string) error {
	item, err := s.api.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspect container %s: %w", containerName, err)
	}

	if _, ok := item.NetworkSettings.Networks[networkName]; ok {
		return nil
	}

	if err := s.api.NetworkConnect(ctx, networkName, containerName, &network.EndpointSettings{}); err != nil {
		return fmt.Errorf("connect container %s to network %s: %w", containerName, networkName, err)
	}

	return nil
}

func (s *Service) RecreateContainer(ctx context.Context, spec ContainerSpec) (ContainerInspect, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return ContainerInspect{}, fmt.Errorf("container name must not be empty")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return ContainerInspect{}, fmt.Errorf("container image must not be empty")
	}

	if spec.Network != "" {
		if err := s.EnsureNetwork(ctx, spec.Network); err != nil {
			return ContainerInspect{}, err
		}
	}

	if _, err := s.api.ContainerInspect(ctx, spec.Name); err == nil {
		timeoutSeconds := 10
		if err := s.api.ContainerStop(ctx, spec.Name, container.StopOptions{Timeout: &timeoutSeconds}); err != nil && !errdefs.IsNotFound(err) {
			return ContainerInspect{}, fmt.Errorf("stop existing container %s: %w", spec.Name, err)
		}
		if err := s.api.ContainerRemove(ctx, spec.Name, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return ContainerInspect{}, fmt.Errorf("remove existing container %s: %w", spec.Name, err)
		}
	} else if !errdefs.IsNotFound(err) {
		return ContainerInspect{}, fmt.Errorf("inspect existing container %s: %w", spec.Name, err)
	}

	config := &container.Config{
		Image:        spec.Image,
		Env:          envSlice(spec.Env),
		Labels:       mapsClone(spec.Labels),
		ExposedPorts: exposedPorts(spec.ExposedPorts, spec.PublishedPorts),
	}
	hostConfig := &container.HostConfig{
		Binds:        bindMounts(spec.Mounts),
		PortBindings: portBindings(spec.PublishedPorts),
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyMode(restartPolicyName(spec.RestartPolicy)),
		},
	}
	networkConfig := &network.NetworkingConfig{}

	if spec.Network != "" {
		hostConfig.NetworkMode = container.NetworkMode(spec.Network)
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			spec.Network: {},
		}
	}

	created, err := s.api.ContainerCreate(ctx, config, hostConfig, networkConfig, nil, spec.Name)
	if err != nil {
		return ContainerInspect{}, fmt.Errorf("create container %s: %w", spec.Name, err)
	}

	if err := s.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return ContainerInspect{}, fmt.Errorf("start container %s: %w", spec.Name, err)
	}

	return s.InspectContainer(ctx, created.ID)
}

func (s *Service) RemoveContainer(ctx context.Context, containerName string) error {
	timeoutSeconds := 10
	if err := s.api.ContainerStop(ctx, containerName, container.StopOptions{Timeout: &timeoutSeconds}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop container %s: %w", containerName, err)
	}
	if err := s.api.ContainerRemove(ctx, containerName, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %s: %w", containerName, err)
	}
	return nil
}

func (s *Service) StreamLogs(ctx context.Context, containerName string, tail int) (io.ReadCloser, error) {
	tailValue := "200"
	if tail > 0 {
		tailValue = strconv.Itoa(tail)
	}

	reader, err := s.api.ContainerLogs(ctx, containerName, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       tailValue,
	})
	if err != nil {
		return nil, fmt.Errorf("stream logs for %s: %w", containerName, err)
	}

	return reader, nil
}

func mapsClone[K comparable, V any](input map[K]V) map[K]V {
	if len(input) == 0 {
		return nil
	}

	cloned := make(map[K]V, len(input))
	for key, value := range input {
		cloned[key] = value
	}

	return cloned
}

func envSlice(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}

	return result
}

func bindMounts(mounts []Mount) []string {
	if len(mounts) == 0 {
		return nil
	}

	result := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		result = append(result, mount.Source+":"+mount.Target)
	}
	sort.Strings(result)
	return result
}

func exposedPorts(list []string, published []PortBinding) nat.PortSet {
	if len(list) == 0 && len(published) == 0 {
		return nil
	}

	result := nat.PortSet{}
	for _, port := range list {
		natPort := mustPort(port, "tcp")
		result[natPort] = struct{}{}
	}
	for _, port := range published {
		natPort := mustPort(port.ContainerPort, protocolOrDefault(port.Protocol))
		result[natPort] = struct{}{}
	}
	return result
}

func portBindings(list []PortBinding) nat.PortMap {
	if len(list) == 0 {
		return nil
	}

	result := nat.PortMap{}
	for _, binding := range list {
		natPort := mustPort(binding.ContainerPort, protocolOrDefault(binding.Protocol))
		result[natPort] = append(result[natPort], nat.PortBinding{
			HostIP:   binding.HostIP,
			HostPort: binding.HostPort,
		})
	}
	return result
}

func mustPort(port, protocol string) nat.Port {
	natPort, err := nat.NewPort(protocol, port)
	if err != nil {
		panic(fmt.Sprintf("invalid port definition %s/%s: %v", port, protocol, err))
	}
	return natPort
}

func protocolOrDefault(protocol string) string {
	if strings.TrimSpace(protocol) == "" {
		return "tcp"
	}
	return strings.ToLower(protocol)
}

func restartPolicyName(policy string) string {
	switch strings.TrimSpace(policy) {
	case "", "unless-stopped":
		return "unless-stopped"
	case "always", "no", "on-failure":
		return policy
	default:
		return "unless-stopped"
	}
}

func ContainsNetwork(inspect ContainerInspect, networkName string) bool {
	return slices.Contains(inspect.Networks, networkName)
}
