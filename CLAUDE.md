# wait-for

`waitfor` is a semantic condition poller — it blocks until one or more conditions are satisfied, then exits 0. It is used in shell scripts, CI pipelines, Kubernetes init containers, and agent workflows.

## Build & Test

```bash
make build       # go build -o bin/waitfor ./cmd/waitfor
make test        # go test ./...
make lint        # golangci-lint run
```

Verification before finishing any change:

```bash
go build ./... && go test ./... && golangci-lint run
```

## Architecture

```
cmd/waitfor/        — Cobra entrypoint; delegates to internal/cli
internal/cli/       — Multi-condition argument parsing; backend wiring
internal/condition/ — One file per backend: http, tcp, file, exec, k8s
internal/runner/    — Timeout, interval, all/any mode, structured output
internal/output/    — text/json formatters (human progress → stderr, JSON → stdout)
internal/expr/      — Minimal JSONPath expression evaluator
```

Key interface — every backend implements this in `internal/condition/condition.go`:

```go
type Condition interface {
    Descriptor() Descriptor
    Check(ctx context.Context) Result
}
```

`Check` returns one of three statuses: `satisfied`, `unsatisfied`, or `fatal`. The runner maps these to exit codes 0, 1, and 3 respectively.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0    | All (or any) conditions satisfied |
| 1    | Timeout expired |
| 2    | Invalid arguments |
| 3    | Fatal condition failure |
| 130  | Cancelled (SIGINT/SIGTERM) |

## Adding a Backend

1. Create `internal/condition/<name>.go` implementing `condition.Condition`.
2. Add parser wiring in `internal/cli/cli.go`.
3. Add table-driven tests in `internal/condition/<name>_test.go`.
4. Backends must return promptly when `ctx` is cancelled.
5. Use `condition.Satisfied`, `condition.Unsatisfied`, or `condition.Fatal` helpers rather than constructing `Result` directly.

## Design Constraints

- CLI parsing is separate from backend implementations — backends must be testable without Cobra.
- The runner is backend-agnostic; all/any mode, timeout, and parallelism live there.
- JSON output goes to stdout only; human progress goes to stderr.
- Kubernetes uses a getter abstraction so tests use client-go fakes; production uses the dynamic client.
- JSON expression support stays minimal unless a concrete use case requires expansion.
- Do not add dependencies that stdlib or existing deps already cover.
