# waitfor

`waitfor` is a semantic condition poller for shell scripts, CI pipelines,
Kubernetes init containers, and agent workflows. It blocks until conditions are
satisfied, then exits `0`. If the timeout expires, it exits `1` and can emit a
structured JSON result.

Human-readable progress is written to stderr. JSON output is written to stdout
without progress lines so it is safe to consume from scripts.

## Examples

```bash
waitfor http https://api.example.com/health --status 200
waitfor http https://api.example.com/health --status 200 --body-contains ok
waitfor tcp localhost:5432
waitfor dns api.example.com --type A --min-count 1
waitfor dns api.example.com --resolver wire --server 1.1.1.1 --type HTTPS --rcode NOERROR
waitfor docker my-container --status running --health healthy
waitfor file /tmp/ready.flag --exists
waitfor file /tmp/lock --deleted
waitfor log /var/log/app.log --contains "server ready"
waitfor log /var/log/app.log --matches "ERROR:.*timeout" --from-start
waitfor exec --output-contains Running -- kubectl get pod myapp
waitfor k8s deployment/myapp --condition Available --namespace prod
```

Multiple conditions are chained with `--` before the next backend:

```bash
waitfor --timeout 10m \
  http https://api.example.com/health \
  -- tcp localhost:5432 \
  -- k8s deployment/myapp --condition Available
```

By default, all conditions must pass. Use `--mode any` when the first satisfied
condition should complete the run.

## CLI

```text
waitfor [flags] <backend> <target> [backend-flags]
waitfor [flags] <backend> ... -- <backend> ...
```

Global flags:

```text
--timeout duration     Global deadline (default: 5m)
--interval duration    Poll interval (default: 2s)
--attempt-timeout duration
                       Per-attempt deadline (default: global remaining time)
--output string        Output format: text|json (default: text)
--mode string          Condition mode: all|any (default: all)
--verbose              Show each attempt
```

Backends:

```text
http URL [--status 200|2xx] [--method GET] [--body text] [--body-file path] [--body-contains text] [--body-matches regex] [--jsonpath expr] [--header K=V] [--insecure] [--no-follow-redirects]
tcp HOST:PORT
dns HOST [--resolver system|wire] [--type A|AAAA|CNAME|TXT|ANY|MX|SRV|NS|CAA|HTTPS|SVCB] [--contains text] [--equals value] [--min-count N] [--absent] [--absent-mode any|nxdomain|nodata] [--server address] [--rcode code] [--transport udp|tcp] [--edns0] [--udp-size N]
docker CONTAINER [--status running] [--health healthy]
exec [--exit-code N] [--output-contains text] [--jsonpath expr] [--cwd path] [--env K=V] [--max-output-bytes N] -- COMMAND [ARGS...]
file PATH [--exists|--deleted|--nonempty] [--contains text]
log PATH (--contains text | --matches regex | --jsonpath expr) [--from-start]
k8s RESOURCE [--condition Ready] [--namespace default] [--jsonpath expr] [--kubeconfig path]
```

`exec` flags must appear before the command separator. `--exit-code` must be
non-negative and defaults to `0`. Everything after `--` belongs to the command:

```bash
waitfor exec --output-contains ready -- /bin/sh -c 'printf ready'
```

For non-exec backends, a literal `--` immediately after a value-taking flag is
treated as that flag's value. Use a second separator to start another condition:

```bash
waitfor file ./ready --contains -- -- http https://api.example.com/health
```

DNS uses Go's standard resolver by default. `--resolver system` is portable and
supports `A`, `AAAA`, `CNAME`, `TXT`, and `ANY`, including absence checks where
"not found" is enough. Use `--resolver wire --server ADDRESS` for lower-level
DNS checks that need exact response codes, NXDOMAIN vs NODATA absence modes,
transport selection, EDNS0, or record types such as `MX`, `SRV`, `NS`, `CAA`,
`HTTPS`, and `SVCB`. `--rcode` accepts known DNS response codes and can be used
by itself to wait for responses such as `SERVFAIL`, `REFUSED`, or `NXDOMAIN`.

Docker polling shells out to the Docker CLI and inspects container state. A
missing Docker binary is fatal; missing containers, daemon connection failures,
or containers in the wrong state remain retryable until the timeout and are
reported with the last observed inspect detail.

## JSON Expressions

The first implementation intentionally supports a small expression subset:

```text
.field
.field.subfield == "value"
.field >= 10
.items[0].name == "first"
{.status.phase}=Running
```

This keeps the core dependency set small and makes it easy to swap in a fuller
expression engine later without changing backend interfaces.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| 0 | Conditions satisfied |
| 1 | Timeout expired before conditions were met |
| 2 | Invalid arguments or configuration |
| 3 | Unrecoverable condition failure |
| 130 | Cancelled by parent context or SIGINT/SIGTERM |

## Design Notes

The code is organized around a small `condition.Condition` interface. Each
backend implements `Descriptor()` and `Check(context.Context) condition.Result`,
while the runner owns timeout, interval, parallelism, all/any mode, and
structured attempt data. `Check` results use explicit statuses:
`satisfied`, `unsatisfied`, or `fatal`.
The runner distinguishes `satisfied`, `timeout`, `cancelled`, and `fatal`
outcomes so scripts can tell a deadline from an interrupted run.
JSON condition records include `backend`, `target`, and `name`; scripts should
prefer `backend` and `target` over parsing the human-readable `name`.

CLI parsing is intentionally separate from the backend implementations. That
keeps backends testable without Cobra and makes multi-condition parsing a narrow
concern. Kubernetes uses a getter abstraction so tests can use client-go fakes
and production can use the dynamic client. DNS follows the same split: ordinary
lookups use the standard library, while `--resolver wire` opts into
`codeberg.org/miekg/dns` v2 for message-level behavior.

## Development Plan

1. Keep the backend interface stable and add richer condition-specific options
   through constructors and parser code.
2. Preserve runner status semantics and per-attempt timeout behavior as new
   backends are added.
3. Expand JSON expression support only when real use cases require it.
4. Add new backends by implementing `condition.Condition`, parser wiring, and
   table-driven tests.
5. Keep runner behavior backend-agnostic so scaling to many conditions remains
   a concurrency and cancellation problem, not a CLI problem.
6. Keep release and CI automation aligned with the supported backend matrix.

## Development

The project targets Go 1.26 and pins the toolchain to Go 1.26.2.

```bash
make test
make build
make lint
make security
make coverage
gocyclo -over 9 $(find . -name '*.go' -not -name '*_test.go')
```

Opt-in black-box suites exercise the compiled binary:

```bash
make test-integration
make test-integration-docker # requires Docker
make test-integration-k8s    # requires kubectl and a current Kubernetes context
```
