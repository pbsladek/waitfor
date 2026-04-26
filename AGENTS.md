# Agent Guide — wait-for

`waitfor` is a semantic condition poller. It blocks until one or more conditions
are satisfied, then exits 0. Used in shell scripts, CI pipelines, Kubernetes
init containers, and agent workflows.

## Quick orientation

```
cmd/waitfor/        entrypoint — signal handling, delegates to internal/cli
internal/cli/       argument parsing, doctor command, backend wiring, exit codes
internal/condition/ one file per backend: http, tcp, dns, docker, file, log, exec, k8s
internal/runner/    polling loop — timeout, interval, backoff/jitter, all/any, parallelism
internal/output/    text/JSON formatters (progress → stderr, JSON → stdout)
internal/expr/      minimal JSONPath evaluator used by http, exec, log, and k8s
e2e/                end-to-end tests that call cli.Execute() directly
integration/        black-box tests that compile and shell-execute the real binary
```

## Build and verify

```bash
make build       # go build -o bin/waitfor ./cmd/waitfor
make test        # go test ./...
make lint        # golangci-lint run ./...
make security    # golangci-lint run --enable=gosec ./...
make coverage    # go test -coverpkg=./... then open coverage.html
```

Always run before finishing any change:

```bash
go build ./... && go test ./... && golangci-lint run ./...
gocyclo -over 9 $(find . -name '*.go' -not -name '*_test.go')
```

For changes that affect the shell-invoked binary, also run:

```bash
make test-integration
```

## Core interface

Every backend implements exactly this in `internal/condition/condition.go`:

```go
type Condition interface {
    Descriptor() Descriptor          // backend name + target, used in output
    Check(ctx context.Context) Result
}
```

`Descriptor().Name` may be overridden by `--name` via `condition.WithName`;
backends should still set stable `Backend` and `Target` values.

`Check` returns one of three terminal values — use the helpers, never construct
`Result` directly:

```go
condition.Satisfied("detail string")
condition.Unsatisfied("detail", err)   // will be retried
condition.Fatal(err)                   // stops the runner immediately
```

## Exit codes

| Code | Meaning                  |
|------|--------------------------|
| 0    | Conditions satisfied     |
| 1    | Timeout expired          |
| 2    | Invalid arguments        |
| 3    | Fatal condition error    |
| 130  | Cancelled (SIGINT)       |
| 143  | Cancelled (SIGTERM)      |

## Quality standards

These are enforced in CI and must hold after every change.

### Test coverage ≥ 90 %

```bash
make coverage   # prints per-function coverage; total is on the last line
```

Coverage is measured with `-coverpkg=./...` so e2e tests count across all
packages. The e2e suite in `e2e/e2e_test.go` calls `cli.Execute()` directly
(not a subprocess) so it contributes instrumentation to every internal package.

When adding a backend, the matching `_test.go` must bring unit coverage of that
file above 85 % before the e2e tests are considered.

### Cyclomatic complexity ≤ 9 per function

```bash
gocyclo -over 9 $(find . -name '*.go' -not -name '*_test.go')
```

Current worst-case production functions are all at 9 or below. Before
introducing new logic, check that no function will exceed 9. If it would,
extract a helper first. Helpers should be package-level functions (not closures
or unexported methods on a struct), so they are independently testable.

Established extraction patterns to follow:

| Domain           | Extracted helper             | Reduces complexity in         |
|------------------|------------------------------|-------------------------------|
| `expr`           | `compareFloat64/Bool/String` | `compareValues`               |
| `expr`           | `traverseField/Indexes`      | `lookupJSONPath`              |
| `runner`         | `validateRunConfig`          | `Run`                         |
| `runner`         | `validateTimingConfig`       | `validateRunConfig`           |
| `runner`         | `validateBackoffConfig`      | `validateRunConfig`           |
| `runner`         | `finalStatus`                | `Run`                         |
| `runner`         | `waitInterval`               | `runCondition`                |
| `runner`         | `buildAttemptEvent`          | `runCondition`                |
| `runner`         | `pollSchedule.next`          | `runCondition`                |
| `condition/http` | `checkResponseBody`          | `HTTPCondition.Check`         |
| `condition/http` | `buildInsecureTransport`     | `HTTPCondition.client`        |
| `condition/exec` | `classifyRunError`           | `ExecCondition.Check`         |
| `condition/exec` | `checkExecOutput`            | `ExecCondition.Check`         |
| `condition/k8s`  | `validateK8sResource`        | `KubernetesCondition.Check`   |
| `condition/k8s`  | `checkK8sNamedCondition`     | `KubernetesCondition.Check`   |
| `condition/dns`  | `checkRCode`                 | `DNSCondition.evaluate`       |
| `condition/dns`  | `checkPresentValues`         | `DNSCondition.evaluate`       |
| `cli`            | `parseBodyContent`           | `parseHTTPCondition`          |
| `cli`            | `parseHTTPHeaders`           | `parseHTTPCondition`          |
| `cli`            | `applyFormatAndMode`         | `parseGlobal`                 |
| `cli`            | `validateGeneralOptions`     | `applyFormatAndMode`          |
| `cli`            | `validateBackoffOptions`     | `validateGeneralOptions`      |
| `cli`            | `parseConditionName`         | `parseCondition`              |
| `cli`            | `validateDNSAbsentOptions`   | `validateDNSWireOptions`      |
| `cli`            | `validateDNSTransportOptions` | `validateDNSWireOptions`      |
| `cli/doctor`     | `parseDoctorOptions`         | `runDoctor`                   |

## Adding a backend

1. Create `internal/condition/<name>.go` — implement `condition.Condition`.
2. Wire it in `internal/cli/cli.go`: add a case to `parseCondition` and write a
   `parse<Name>Condition(segment []string)` function.
3. Add `internal/condition/<name>_test.go` — table-driven unit tests, no Cobra.
4. Add e2e cases in `e2e/e2e_test.go` — at least: satisfied, timeout, invalid
   args → `ExitInvalid`, fatal path → `ExitFatal`.
5. Add black-box coverage in `integration/blackbox_test.go` when behavior must
   be validated through the real shell-invoked binary.
6. Return promptly when `ctx` is cancelled; never block after `ctx.Done()`.
7. Use `condition.Fatal` only for errors that cannot resolve on retry (missing
   binary, bad credentials, invalid config). Network errors are `Unsatisfied`.

### Checklist for a new backend

- [ ] `Descriptor()` returns `Backend` and `Target` fields
- [ ] `Check` uses `ctx` throughout (pass to all blocking calls)
- [ ] Error paths: `Fatal` for permanent, `Unsatisfied` for transient
- [ ] No global state; all config lives on the struct
- [ ] Unit tests do not require a running external service (use fakes/stubs)
- [ ] Black-box tests cover CLI behavior that depends on process execution,
      shell quoting, streams, or real polling
- [ ] `gocyclo` score ≤ 9 for every new function
- [ ] Coverage of the new file ≥ 85 % from unit tests alone

## Key design constraints

- **CLI parsing is separate from backends.** `internal/condition` has no
  dependency on Cobra or pflag. Backends are testable by constructing the struct
  directly.
- **The runner is backend-agnostic.** Parallelism, timeout, all/any mode, and
  the polling loop all live in `internal/runner`. Backends know nothing about
  retries, backoff, jitter, `--successes`, or `--stable-for`.
- **JSON output goes to stdout; human progress goes to stderr.** Never swap
  these. The printer selects the channel automatically based on `--output json`.
- **Condition labels are wrappers, not backend fields.** `--name` is implemented
  by `condition.WithName`; do not add label fields to individual backends.
- **Doctor is a CLI command, not a backend.** `waitfor doctor` reports local
  environment support and should not enter the runner.
- **Kubernetes uses a `KubernetesGetter` interface.** Production code uses the
  dynamic client; tests inject a `fake.NewSimpleDynamicClient`. Do not call the
  real API in unit tests.
- **DNS has two resolver modes.** `system` uses the Go standard library and
  should remain the default for portable A/AAAA/CNAME/TXT/ANY checks. `wire`
  uses `codeberg.org/miekg/dns` v2 for message-level behavior such as response
  codes, NXDOMAIN vs NODATA, EDNS0, transport selection, and MX/SRV/NS/CAA/
  HTTPS/SVCB records. Keep wire-only behavior behind `--resolver wire`.
- **Docker uses the Docker CLI boundary.** Missing Docker is fatal because
  retries cannot fix it. Missing containers, inspect failures, and state or
  health mismatches are retryable unless the configuration itself is invalid.
- **Log polling keeps state in the condition.** It handles tailing, rotation,
  exclusion, regex/string matching, JSON expressions, and minimum match counts.
- **`expr` stays minimal.** The JSONPath evaluator covers the subset needed by
  `--jsonpath`. Do not add operators or syntax without a concrete use case.
- **No new dependencies** unless the stdlib and existing deps genuinely cannot
  solve the problem. Prefer opt-in dependency paths when only advanced behavior
  needs the dependency, as with DNS wire mode.

## Testing patterns

### Unit tests (condition package)
Construct the struct directly and call `Check(t.Context())`:

```go
cond := condition.NewHTTP(server.URL)
cond.BodyJSONExpr = expr.MustCompile(".ready == true")
result := cond.Check(t.Context())
if result.Status != condition.CheckSatisfied { ... }
```

### E2e tests (e2e package)
Call `cli.Execute()` with real args; use short timeouts for failure paths:

```go
mustCode(t, cli.ExitSatisfied, "http", server.URL, "--jsonpath", ".ready == true")
mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "http", server.URL)
mustCode(t, cli.ExitFatal, "exec", "--", "/no/such/binary")
```

### Internal CLI tests (cli package, same package as the code)
Call unexported parse functions directly to exercise error paths without
needing a network or filesystem:

```go
_, err := parseKubernetesCondition([]string{"k8s", "pod/a", "--jsonpath", " "})
// err must be non-nil (blank jsonpath fails compilation)
```

### Integration tests
Black-box tests in `integration/blackbox_test.go` compile and execute the real
binary when `WAITFOR_BLACKBOX=1` is set. Docker cases additionally require
`WAITFOR_BLACKBOX_DOCKER=1`; Kubernetes cases require `WAITFOR_BLACKBOX_K8S=1`
and a real cluster context. Keep Docker and Kubernetes tests skipped by default
so `go test ./...` does not require Docker, kind, or a cluster.

### Release and Docker automation
Version tags are created with:

```bash
make release-tag VERSION=v0.1.0
```

The tag must point at a commit containing `.github/workflows/release.yml`; the
Makefile checks this before tagging. Existing tags can be released without the
GitHub UI via `make release-existing VERSION=v0.1.0`.

Docker publish builds are intentionally split by platform. The workflow builds
`linux/amd64` on `ubuntu-latest` and `linux/arm64` on `ubuntu-24.04-arm`, pushes
per-platform digests, then creates a multi-arch manifest. Preserve this shape so
arm64 builds do not fall back to QEMU.

## Common mistakes to avoid

- Returning `Fatal` for a network timeout — use `Unsatisfied` so the runner
  retries. Only use `Fatal` when retrying is pointless.
- Treating DNS `NXDOMAIN` and `NODATA` as distinguishable in `system` resolver
  mode — use `wire` mode when precise response classification is required.
- Blocking in `Check` after `ctx.Done()` fires — the runner will not cancel a
  goroutine that ignores context; it will hang until the global timeout.
- Writing to stdout from a backend — all output is the runner's responsibility.
- Using `math/rand` for jitter — `gosec` flags it; use `crypto/rand` or a
  deterministic test seam.
- Adding a `sync.Mutex` to a condition struct when the runner guarantees each
  `Check` call is serialised per-condition (use `sync.Once` for lazy init only).
- Breaking the `Condition` interface to pass extra information — put it in the
  struct fields set before `Check` is called.
