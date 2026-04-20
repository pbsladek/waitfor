package condition

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestDockerConditionRunningSatisfied(t *testing.T) {
	cond := NewDocker("api")
	cond.Inspect = func(_ context.Context, container string) (DockerState, error) {
		if container != "api" {
			t.Fatalf("container = %q, want api", container)
		}
		return DockerState{Status: "running", Running: true}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestDockerConditionStatusUnsatisfied(t *testing.T) {
	cond := NewDocker("api")
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{Status: "created"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestDockerConditionAnyStatus(t *testing.T) {
	cond := NewDocker("api")
	cond.Status = "any"
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{Status: "exited"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestDockerConditionHealthy(t *testing.T) {
	cond := NewDocker("api")
	cond.Health = "healthy"
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{
			Status: "running",
			Health: &DockerHealth{Status: "healthy"},
		}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestDockerConditionHealthMissing(t *testing.T) {
	cond := NewDocker("api")
	cond.Health = "healthy"
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{Status: "running"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestDockerConditionNoHealthSatisfied(t *testing.T) {
	cond := NewDocker("api")
	cond.Health = "none"
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{Status: "running"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestDockerConditionInspectErrorUnsatisfied(t *testing.T) {
	cond := NewDocker("missing")
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{}, fmt.Errorf("no such container")
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestDockerConditionMissingDockerFatal(t *testing.T) {
	cond := NewDocker("api")
	cond.Inspect = func(context.Context, string) (DockerState, error) {
		return DockerState{}, exec.ErrNotFound
	}

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestDockerConditionContextCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cond := NewDocker("api")
	cond.Inspect = func(ctx context.Context, _ string) (DockerState, error) {
		return DockerState{}, ctx.Err()
	}

	result := cond.Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err == nil || result.Err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", result.Err)
	}
}

func TestDockerConditionEmptyContainerFatal(t *testing.T) {
	result := NewDocker(" ").Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestDockerConditionInvalidDirectConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*DockerCondition)
	}{
		{"status", func(c *DockerCondition) { c.Status = "warm" }},
		{"health", func(c *DockerCondition) { c.Health = "warm" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewDocker("api")
			cond.Inspect = func(context.Context, string) (DockerState, error) {
				t.Fatal("Inspect should not be called for invalid config")
				return DockerState{}, nil
			}
			tt.setup(cond)

			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestClassifyDockerInspectError(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "daemon",
			output: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			want:   "docker daemon not running",
		},
		{
			name:   "missing container",
			output: "Error: No such object: api",
			want:   "docker container not found",
		},
		{
			name:   "generic with output",
			output: "permission denied",
			want:   "docker inspect failed: permission denied",
		},
		{
			name:   "generic empty",
			output: "",
			want:   "docker inspect failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyDockerInspectError(fmt.Errorf("exit status 1"), tt.output)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDockerInspectOutputTruncatesLongErrors(t *testing.T) {
	got := dockerInspectOutput(strings.Repeat("x", 600))
	if len(got) > 520 {
		t.Fatalf("len(output) = %d, want capped output", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("output = %q, want truncation marker", got)
	}
}

func TestDockerDescriptor(t *testing.T) {
	d := NewDocker("api").Descriptor()
	if d.Backend != "docker" || d.Target != "api" {
		t.Fatalf("descriptor = %+v", d)
	}
}
