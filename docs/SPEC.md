# waitfor Implementation Spec

## Scope

`waitfor` is a Go CLI that polls one or more semantic conditions until they are
satisfied, a timeout expires, or an unrecoverable condition error occurs.

Runs have one final status:

| Status | Meaning |
| ------ | ------- |
| `satisfied` | Required condition mode completed successfully. |
| `timeout` | The global deadline expired. |
| `cancelled` | The parent context was cancelled, including SIGINT/SIGTERM from the CLI entrypoint. |
| `fatal` | A condition reported an unrecoverable error. |

## Current Backends

| Backend | Syntax | Notes |
| ------- | ------ | ----- |
| `http` | `waitfor http URL --status 2xx --body-contains ok --jsonpath '.ready == true'` | Supports method, exact status, status classes, headers, request bodies, body substring, body regex, minimal JSON expressions, redirect control, and insecure TLS. |
| `tcp` | `waitfor tcp HOST:PORT` | Opens and closes a TCP connection. |
| `dns` | `waitfor dns api.internal --type A --min-count 1` | Uses the system resolver by default or `--resolver wire` for protocol-level checks via `codeberg.org/miekg/dns`; supports exact values, minimum answer counts, absence, selected RCODEs, transport options, and richer RR types. |
| `docker` | `waitfor docker postgres --status running --health healthy` | Uses `docker inspect` for local container status and health checks. |
| `exec` | `waitfor exec --output-contains ok -- COMMAND` | Runs a command with context cancellation and checks exit code, output substring, or JSON expression. Supports cwd, env, and output limits. |
| `file` | `waitfor file PATH --exists` | Supports `--exists`, `--deleted`, `--nonempty`, and substring checks. |
| `log` | `waitfor log PATH --contains "ready"` | Tails a file and returns satisfied when a matching line appears. Tracks byte offset across polls; detects rotation via inode change. Supports substring, regex, and JSON expression matchers (AND semantics). Default behaviour skips existing content; `--from-start` scans from byte 0. |
| `k8s` | `waitfor k8s deployment/myapp --condition Available` | Uses client-go dynamic client and supports common built-in Kubernetes resources. |

## CLI Grammar

Global flags must appear before the first backend. Multiple conditions are
separated with `--` followed by a backend name. `exec` uses `--` to separate
waitfor's exec flags from the command; after that separator, tokens are passed to
the command unchanged.

```text
waitfor [global-flags] condition [-- condition...]
condition := backend backend-args backend-flags
exec-condition := exec [exec-flags] -- command [args...]
```

Because `-- backend` is the condition separator, an exec command that needs the
literal token pair `-- file`, `-- http`, `-- tcp`, `-- dns`, `-- docker`,
`-- exec`, or `-- k8s` cannot be followed unambiguously by more waitfor
conditions.

A literal `--` used as the value for a value-taking backend flag is treated as
that flag's value, not as a condition separator. To continue with another
condition after such a value, provide a second separator: `--contains -- --
http https://example.com`.

Global flags:

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--timeout duration` | `5m` | Global deadline for the run. |
| `--interval duration` | `2s` | Delay between attempts. |
| `--attempt-timeout duration` | global remaining time | Per-condition attempt deadline. |
| `--output text|json` | `text` | Output format. |
| `--mode all|any` | `all` | Completion mode across conditions. |
| `--verbose` | `false` | Emit per-attempt human progress in text mode. |

Backend grammar:

```text
http URL [--status CODE|CLASS] [--method METHOD] [--header K=V]
         [--body TEXT | --body-file PATH] [--body-contains TEXT]
         [--body-matches REGEX] [--jsonpath EXPR]
         [--insecure] [--no-follow-redirects]

tcp HOST:PORT

dns HOST [--type TYPE] [--resolver system|wire] [--contains TEXT]
         [--equals VALUE ...] [--min-count N] [--absent]
         [--absent-mode any|nxdomain|nodata] [--server ADDRESS]
         [--rcode CODE] [--transport udp|tcp] [--edns0] [--udp-size N]

docker CONTAINER [--status STATUS] [--health HEALTH]

exec [--exit-code N] [--output-contains TEXT] [--jsonpath EXPR]
     [--cwd PATH] [--env K=V ...] [--max-output-bytes N] -- COMMAND [ARGS...]

file PATH [exists|deleted|nonempty] [--contains TEXT]

k8s RESOURCE [--condition TYPE] [--jsonpath EXPR]
             [--namespace NAMESPACE] [--kubeconfig PATH]
```

## Backend Contracts

### HTTP

`http` performs one request per check. Network errors, non-matching statuses,
and body matcher failures are retryable. Invalid URLs, invalid status matchers,
invalid headers, invalid body regexes, unreadable body files, or invalid JSON
expressions are argument errors during parsing. Runtime JSON parse or expression
evaluation failures are retryable because a service may emit non-JSON startup
output before becoming ready.

The default method is `GET`, default status matcher is `200`, redirects are
followed unless `--no-follow-redirects` is set, and TLS verification is enabled
unless `--insecure` is set. `--body` and `--body-file` are mutually exclusive.

### TCP

`tcp` validates `HOST:PORT` during parsing. Each check opens a TCP connection
with the attempt context and closes it immediately after success. Dial failures
are retryable.

### DNS

`dns` validates host names before lookup. Names may be absolute with a trailing
dot, each label must be 63 bytes or fewer, and the full name must fit the DNS
wire limit after trimming the trailing dot. Validation failures are fatal when
the condition is constructed directly and argument errors through the CLI.

Supported record types are `A`, `AAAA`, `CNAME`, `TXT`, `ANY`, `MX`, `SRV`,
`NS`, `CAA`, `HTTPS`, and `SVCB`. The default type is `A`.

Resolver modes:

| Mode | Behavior |
| ---- | -------- |
| `system` | Default. Uses Go's standard resolver. Supports `A`, `AAAA`, `CNAME`, `TXT`, and `ANY`. It can treat resolver "not found" errors as absence for `--absent`, but it does not promise a portable distinction between NXDOMAIN and NODATA. |
| `wire` | Uses `codeberg.org/miekg/dns` v2 and requires `--server`. Supports all listed record types, exact RCODE checks, NXDOMAIN vs NODATA absence, UDP/TCP selection, EDNS0, and UDP payload size. |

DNS matchers:

| Option | Meaning |
| ------ | ------- |
| `--contains TEXT` | At least one answer string must contain `TEXT`. |
| `--equals VALUE` | Required exact answer value. Repeatable; every requested value must be present. CNAME and NS equality is case-insensitive and ignores a single trailing dot. |
| `--min-count N` | At least `N` answers must be present. `N` must be non-negative. |
| `--absent` | Wait until the queried name or record type is absent. Cannot be combined with `--contains`, `--equals`, or `--min-count`. |
| `--absent-mode any` | Default. Satisfied by NXDOMAIN, NODATA, no answer values, or system resolver "not found" errors. |
| `--absent-mode nxdomain` | Wire-only. Satisfied only by NXDOMAIN. |
| `--absent-mode nodata` | Wire-only. Satisfied only by NOERROR with no matching answers. |
| `--rcode CODE` | Wire-only. Response code must be a known DNS RCODE and match, such as `NOERROR`, `SERVFAIL`, `REFUSED`, or `NXDOMAIN`. If no other positive matcher is set, the RCODE match alone satisfies the condition. |

Wire resolver options are valid only with `--resolver wire`. `--server` accepts
hostnames, IPv4 addresses, bracketed IPv6 addresses, bare IPv6 addresses, or
explicit `host:port`; the port defaults to `53`. `--transport` is `udp` by
default. If a UDP response is truncated, the wire resolver retries the query
over TCP. `--edns0` enables an OPT record; `--udp-size` sets the EDNS0 UDP
payload size and must be between `0` and `65535`.

Wire answers are filtered to the requested RR type before value matching, except
for `ANY`, which exposes every answer. CNAME records in an A/AAAA response do
not satisfy A/AAAA value checks unless a matching address answer is also present.
Canonical answer values are:

| Type | Value format |
| ---- | ------------ |
| `A`, `AAAA` | IP string, such as `192.0.2.10` or `2001:db8::1`. |
| `CNAME`, `NS` | DNS name with the library's trailing-dot form. Equality ignores case and one trailing dot. |
| `TXT` | TXT chunks joined without separators. |
| `MX` | `preference exchange`, such as `10 mail.example.test.` |
| `SRV` | `priority weight port target`, such as `1 2 443 target.example.test.` |
| `CAA` | `flag tag value`, such as `0 issue letsencrypt.org`. |
| `HTTPS`, `SVCB` | DNS library SVCB data string, such as `1 svc.example.test. alpn="h2"`. |

DNS lookup failures are retryable except for validation errors. The backend does
not write output; answer details flow through the normal runner event and result
paths.

### Docker

`docker` shells out to:

```text
docker inspect --type container --format "{{json .State}}" CONTAINER
```

The command output is parsed as Docker's container `.State` object. A missing
Docker binary is fatal. Missing containers, daemon connection failures, inspect
errors, and non-matching state or health are retryable until the global timeout.
Inspect stderr/stdout is capped and included in the last observed error detail.

Valid statuses are `any`, `created`, `running`, `paused`, `restarting`,
`removing`, `exited`, and `dead`. The default is `running`; `any` disables the
status check. Valid health values are `healthy`, `unhealthy`, `starting`, and
`none`; the empty default disables the health check. `none` means the container
has no health object.

### Exec

`exec` starts the requested command with the attempt context. A missing binary or
spawn failure is fatal. Non-matching exit codes, output substring failures, and
JSON expression failures are retryable. Output may be capped with
`--max-output-bytes`. `--exit-code` must be non-negative and defaults to `0`.
On Unix-like platforms, commands are started in a separate process group so
attempt cancellation kills shell descendants as well as the direct child.

### File

`file` checks local filesystem state. The default state is `exists`. `deleted`
is satisfied by a missing path. `nonempty` requires an existing file with size
greater than zero. `--contains` reads the file and requires a substring match.
Missing paths, empty files, and substring mismatches are retryable unless the
requested state is `deleted`.

### Kubernetes

`k8s` resources use `kind/name` syntax. Supported kinds are `pod`, `service`,
`deployment`, `replicaset`, `statefulset`, `daemonset`, `job`, and `namespace`.
All supported kinds are namespaced except `namespace`.

The backend uses the client-go dynamic client in production and a
`KubernetesGetter` test interface in unit tests. API lookup errors and missing
resources are retryable. Unsupported kinds and malformed resource strings are
fatal when constructed directly and argument errors through the CLI.

`--condition TYPE` checks `.status.conditions[]` for a matching `type` with
`status == "True"`. `--jsonpath EXPR` evaluates the minimal expression language
against the unstructured object. `--condition` and `--jsonpath` are mutually
exclusive.

## Core Contract

Every backend implements:

```go
type Condition interface {
    Descriptor() Descriptor
    Check(ctx context.Context) Result
}

type Result struct {
    Status CheckStatus
    Detail string
    Err    error
}
```

`Check` must be idempotent, safe to call repeatedly, and return promptly when
its context is cancelled. Retryable failures return `CheckUnsatisfied`; fatal
configuration or spawn failures return `CheckFatal`.

## Runner

The runner owns all polling behavior:

```go
type Config struct {
    Conditions        []condition.Condition
    Timeout           time.Duration
    Interval          time.Duration
    PerAttemptTimeout time.Duration
    Mode              Mode
    OnAttempt         func(AttemptEvent)
}
```

The runner executes conditions concurrently. `ModeAll` waits for every condition
to satisfy. `ModeAny` cancels remaining work after the first satisfied condition.
Fatal condition errors take precedence over satisfaction if both are recorded in
the same run. A per-attempt timeout of zero means each check receives the global
run context directly. If a per-attempt timeout is larger than the global
timeout, the effective per-attempt timeout is normalized to the global timeout.

## Output

Text output is optimized for humans. JSON output is stable for scripts:

```json
{
  "status": "satisfied",
  "satisfied": true,
  "mode": "all",
  "elapsed_seconds": 1.2,
  "timeout_seconds": 300.0,
  "interval_seconds": 2.0,
  "conditions": [
    {
      "backend": "tcp",
      "target": "localhost:5432",
      "name": "tcp localhost:5432",
      "satisfied": true,
      "attempts": 2,
      "elapsed_seconds": 1.0,
      "detail": "connection established"
    }
  ]
}
```

Human-readable progress and text summaries are emitted on stderr. JSON output is
emitted on stdout without progress lines. Timeout, fatal, and cancelled text
summaries list each unsatisfied condition with the last observed error when
available, otherwise the last observed detail. JSON condition records carry both
`detail` and `last_error` when present.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| 0 | Conditions satisfied |
| 1 | Timeout expired |
| 2 | Invalid arguments or configuration |
| 3 | Unrecoverable condition failure |
| 130 | Cancelled by parent context or SIGINT/SIGTERM |

## Growth Rules

New backends should add one condition type, parser wiring, unit tests for `Check`,
and at least one CLI-level test. Backend packages must not own polling loops,
sleeping, output formatting, or process exit behavior.

Production functions must stay at cyclomatic complexity 9 or below. If a new
backend needs branching for validation or response classification, extract
package-level helpers and cover them directly. Unit tests must not require real
external services; use fakes, injected functions, or local test servers. Real
binary and cluster coverage belongs in `integration/blackbox_test.go` behind
opt-in environment variables such as `WAITFOR_BLACKBOX=1` and
`WAITFOR_BLACKBOX_DOCKER=1` and `WAITFOR_BLACKBOX_K8S=1`. CI should publish
coverage artifacts, enforce total coverage at or above 90%, and keep security
linting enabled separately from the normal lint pass.
