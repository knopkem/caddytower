package projects

import (
	"context"
	"fmt"
	"strings"

	"caddytower/internal/dockerx"

	"github.com/google/uuid"
)

const (
	controllerUpdateSocketPath = "/var/run/docker.sock"
	controllerUpdateAction     = "controller.update"
)

type controllerDocker interface {
	PullImage(context.Context, string) error
	InspectContainer(context.Context, string) (dockerx.ContainerInspect, error)
	RecreateContainer(context.Context, dockerx.ContainerSpec) (dockerx.ContainerInspect, error)
}

func SelfUpdateController(ctx context.Context, docker controllerDocker, containerName, targetImage string) error {
	if docker == nil {
		return fmt.Errorf("docker control unavailable")
	}
	containerName = strings.TrimSpace(containerName)
	targetImage = strings.TrimSpace(targetImage)
	if containerName == "" {
		return fmt.Errorf("container name must not be empty")
	}
	if targetImage == "" {
		return fmt.Errorf("target image must not be empty")
	}

	inspect, err := docker.InspectContainer(ctx, containerName)
	if err != nil {
		return err
	}
	if err := docker.PullImage(ctx, targetImage); err != nil {
		return err
	}

	env := envMap(inspect.Env)
	if len(env) == 0 {
		env = map[string]string{}
	}
	env["CADDYTOWER_IMAGE"] = targetImage

	spec := dockerx.ContainerSpec{
		Name:           containerName,
		Image:          targetImage,
		Command:        append([]string(nil), inspect.Command...),
		Env:            env,
		Labels:         cloneStringMap(inspect.Labels),
		Mounts:         append([]dockerx.Mount(nil), inspect.Mounts...),
		PublishedPorts: append([]dockerx.PortBinding(nil), inspect.PublishedPorts...),
		RestartPolicy:  inspect.RestartPolicy,
	}
	if len(inspect.Networks) > 0 {
		spec.Network = inspect.Networks[0]
	}
	if _, err := docker.RecreateContainer(ctx, spec); err != nil {
		return err
	}
	return nil
}

func (s *Service) ControllerContainer(ctx context.Context) (dockerx.ContainerInspect, error) {
	if s.docker == nil {
		return dockerx.ContainerInspect{}, fmt.Errorf("docker control unavailable")
	}
	return s.docker.InspectContainer(ctx, controllerContainerName)
}

func (s *Service) StartControllerUpdate(ctx context.Context, targetImage, userID string) error {
	if s.docker == nil {
		return fmt.Errorf("docker control unavailable")
	}
	targetImage = strings.TrimSpace(targetImage)
	if targetImage == "" {
		return fmt.Errorf("target image must not be empty")
	}
	if err := s.docker.PullImage(ctx, targetImage); err != nil {
		return err
	}

	updaterName := "caddytower-updater-" + strings.ToLower(uuid.NewString())
	if s.store != nil && userID != "" {
		if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, controllerUpdateAction, "controller:"+controllerContainerName, map[string]any{
			"target_image": targetImage,
			"helper":       updaterName,
		}); err != nil {
			return err
		}
	}
	spec := dockerx.ContainerSpec{
		Name:          updaterName,
		Image:         targetImage,
		Command:       []string{"self-update", "--container-name", controllerContainerName, "--target-image", targetImage},
		Env:           helperEnv(s.cfg.DockerHost),
		Mounts:        []dockerx.Mount{{Source: controllerUpdateSocketPath, Target: controllerUpdateSocketPath}},
		RestartPolicy: "no",
		AutoRemove:    true,
	}
	if _, err := s.docker.RecreateContainer(ctx, spec); err != nil {
		return fmt.Errorf("start controller update helper: %w", err)
	}
	return nil
}

func helperEnv(dockerHost string) map[string]string {
	dockerHost = strings.TrimSpace(dockerHost)
	if dockerHost == "" {
		return nil
	}
	return map[string]string{
		"DOCKER_HOST": dockerHost,
	}
}

func envMap(values []string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, current, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		result[key] = current
	}
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
