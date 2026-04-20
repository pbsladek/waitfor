package condition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type DockerCondition struct {
	Container string
	Status    string
	Health    string
	Inspect   func(context.Context, string) (DockerState, error)
}

type DockerState struct {
	Status  string        `json:"Status"`
	Running bool          `json:"Running"`
	Health  *DockerHealth `json:"Health"`
}

type DockerHealth struct {
	Status string `json:"Status"`
}

func NewDocker(container string) *DockerCondition {
	return &DockerCondition{Container: container, Status: "running"}
}

func (c *DockerCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "docker", Target: c.Container}
}

func (c *DockerCondition) Check(ctx context.Context) Result {
	if strings.TrimSpace(c.Container) == "" {
		return Fatal(fmt.Errorf("docker container is required"))
	}
	if !validDockerStatus(c.status()) {
		return Fatal(fmt.Errorf("invalid docker status %q", c.Status))
	}
	if !validDockerHealth(c.health()) {
		return Fatal(fmt.Errorf("invalid docker health %q", c.Health))
	}
	state, err := c.inspect(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("docker command not found"))
		}
		return Unsatisfied("", err)
	}
	if result := checkDockerStatus(state, c.status()); result != nil {
		return *result
	}
	if result := checkDockerHealth(state, c.Health); result != nil {
		return *result
	}
	return Satisfied(dockerDetail(state))
}

func (c *DockerCondition) inspect(ctx context.Context) (DockerState, error) {
	if c.Inspect != nil {
		return c.Inspect(ctx, c.Container)
	}
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", "{{json .State}}", c.Container)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return DockerState{}, classifyDockerInspectError(err, string(out))
	}
	var state DockerState
	if err := json.Unmarshal(out, &state); err != nil {
		return DockerState{}, fmt.Errorf("parse docker inspect output: %w", err)
	}
	return state, nil
}

func (c *DockerCondition) status() string {
	if c.Status == "" {
		return "running"
	}
	return strings.ToLower(c.Status)
}

func (c *DockerCondition) health() string {
	return strings.ToLower(c.Health)
}

func validDockerStatus(status string) bool {
	switch status {
	case "any", "created", "running", "paused", "restarting", "removing", "exited", "dead":
		return true
	default:
		return false
	}
}

func validDockerHealth(health string) bool {
	switch health {
	case "", "healthy", "unhealthy", "starting", "none":
		return true
	default:
		return false
	}
}

func checkDockerStatus(state DockerState, want string) *Result {
	if want == "any" {
		return nil
	}
	if strings.ToLower(state.Status) == want {
		return nil
	}
	detail := fmt.Sprintf("status %s, expected %s", state.Status, want)
	r := Unsatisfied(detail, errors.New(detail))
	return &r
}

func checkDockerHealth(state DockerState, want string) *Result {
	want = strings.ToLower(want)
	if want == "" {
		return nil
	}
	if want == "none" {
		if state.Health == nil {
			return nil
		}
		return dockerUnsatisfiedHealth(state.Health.Status, want)
	}
	if state.Health == nil {
		return dockerUnsatisfiedHealth("none", want)
	}
	if strings.ToLower(state.Health.Status) == want {
		return nil
	}
	return dockerUnsatisfiedHealth(state.Health.Status, want)
}

func dockerUnsatisfiedHealth(got, want string) *Result {
	detail := fmt.Sprintf("health %s, expected %s", got, want)
	r := Unsatisfied(detail, errors.New(detail))
	return &r
}

func dockerDetail(state DockerState) string {
	detail := "status " + state.Status
	if state.Health != nil {
		detail += ", health " + state.Health.Status
	}
	return detail
}

func classifyDockerInspectError(err error, output string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return err
	}
	detail := dockerInspectOutput(output)
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "cannot connect to the docker daemon"),
		strings.Contains(lower, "is the docker daemon running"):
		return fmt.Errorf("docker daemon not running: %s", detail)
	case strings.Contains(lower, "no such object"),
		strings.Contains(lower, "no such container"):
		return fmt.Errorf("docker container not found: %s", detail)
	case detail != "":
		return fmt.Errorf("docker inspect failed: %s", detail)
	default:
		return fmt.Errorf("docker inspect failed: %w", err)
	}
}

func dockerInspectOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	const maxDockerErrorDetail = 500
	if len(output) <= maxDockerErrorDetail {
		return output
	}
	return output[:maxDockerErrorDetail] + "...(truncated)"
}
