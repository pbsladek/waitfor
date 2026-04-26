# wait-for

`waitfor` is a semantic condition poller for shell scripts, CI pipelines,
Kubernetes init containers, and agent workflows. It blocks until one or more
conditions are satisfied, then exits 0.

## Build And Verify

```bash
make build       # go build -o bin/waitfor ./cmd/waitfor
make test        # go test ./...
make lint        # golangci-lint run ./...
make security    # golangci-lint run --enable=gosec ./...
make coverage    # go test -coverpkg=./... then open coverage.html
```

Always run before finishing code changes:

```bash
go build ./... && go test ./... && golangci-lint run ./...
gocyclo -over 9 $(find . -name '*.go' -not -name '*_test.go')
```

For changes that affect the actual CLI process, also run:

```bash
make test-integration
```

## Architecture

```text
cmd/waitfor/        entrypoint; signal handling; delegates to internal/cli
internal/cli/       argument parsing, doctor command, backend wiring, exit codes
internal/condition/ one file per backend: http, tcp, dns, docker, file, log, exec, k8s
internal/runner/    polling loop: timeout, interval, backoff/jitter, all/any, parallelism
internal/output/    text/JSON formatters; progress to stderr, JSON to stdout
internal/expr/      minimal JSONPath evaluator used by http, exec, log, and k8s
e2e/                tests that call cli.Execute() directly
integration/        black-box tests that compile and shell-execute the real binary
```

## Core Interface

Every backend implements `condition.Condition`:

```go
type Condition interface {
    Descriptor() Descriptor
    Check(ctx context.Context) Result
}
```

Use result helpers instead of constructing `Result` directly:

```go
condition.Satisfied("detail")
condition.Unsatisfied("detail", err)
condition.Fatal(err)
```

`Descriptor().Name` may be overridden by `--name` through
`condition.WithName`; backends should still set stable `Backend` and `Target`
values.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| 0 | Conditions satisfied |
| 1 | Timeout expired |
| 2 | Invalid arguments |
| 3 | Fatal condition failure |
| 130 | Cancelled by SIGINT |
| 143 | Cancelled by SIGTERM |

## Quality Standards

- Coverage must stay at or above 90% with `make coverage`.
- Production functions must stay at `gocyclo` score 9 or below.
- Add package-level helpers before validation, parsing, or polling functions grow.
- Unit tests should construct condition structs directly and avoid external
  services.
- Black-box integration tests should cover process behavior: shell invocation,
  streams, quoting, real binary polling, Docker, and Kubernetes where applicable.

## Design Constraints

- CLI parsing is separate from backend implementations; backends must be testable
  without Cobra or pflag.
- The runner is backend-agnostic. Backends know nothing about retries, backoff,
  jitter, all/any mode, `--successes`, or `--stable-for`.
- JSON output goes to stdout only. Human progress goes to stderr.
- Condition labels are wrappers, not backend fields. `--name` is implemented by
  `condition.WithName`.
- `waitfor doctor` is a CLI command, not a backend, and should not enter the
  runner.
- Kubernetes uses a getter abstraction so unit tests use client-go fakes.
- DNS defaults to the stdlib resolver. Wire-only DNS behavior stays behind
  `--resolver wire`.
- Docker polling uses the Docker CLI boundary. Missing Docker is fatal; missing
  containers and non-matching states are retryable.
- Log polling owns its file offset state and handles tailing, rotation, filters,
  JSON expressions, and minimum match counts.
- Do not add dependencies when stdlib or existing deps can reasonably solve the
  problem.

## Release And Docker Automation

Create releases with:

```bash
make release-tag VERSION=v0.1.0
```

The tag must point at a commit containing `.github/workflows/release.yml`; the
Makefile checks this before tagging. Existing tags can be released without the
GitHub UI via:

```bash
make release-existing VERSION=v0.1.0
```

Docker publish builds are split by platform. `linux/amd64` builds on
`ubuntu-latest`; `linux/arm64` builds on `ubuntu-24.04-arm`; the workflow then
creates the multi-arch manifest from per-platform digests. Preserve this shape
so arm64 does not fall back to QEMU.

## Common Mistakes

- Returning `Fatal` for transient network or service errors.
- Blocking in `Check` after `ctx.Done()` fires.
- Writing to stdout from a backend.
- Treating system-resolver DNS `NXDOMAIN` and `NODATA` as distinguishable.
- Using `math/rand` for jitter; `gosec` flags it, so use `crypto/rand`.
- Adding label fields to every backend instead of using `condition.WithName`.
