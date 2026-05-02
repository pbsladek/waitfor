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

| Backend | Example | Notes |
| ------- | ------- | ----- |
| `http` | `waitfor http URL --status 2xx --body-contains ok` | HTTP request with configurable method, status, headers, body matchers, and TLS options. |
| `tcp` | `waitfor tcp HOST:PORT` | Opens and immediately closes a TCP connection. |
| `dns` | `waitfor dns HOST --type A --min-count 1` | System resolver by default; `--resolver wire` for protocol-level checks. |
| `docker` | `waitfor docker CONTAINER --status running --health healthy` | Shells out to `docker inspect` for container state and health. |
| `exec` | `waitfor exec --output-contains ok -- COMMAND [ARGS...]` | Runs a command and checks exit code, output substring, or JSON expression. |
| `file` | `waitfor file PATH --exists` | Checks filesystem state: exists, deleted, or non-empty; optional substring match. |
| `log` | `waitfor log PATH --contains "ready"` | Tails a file and matches lines using substring, regex, and/or JSON expression. Tracks byte offset across polls; detects log rotation. |
| `k8s` | `waitfor k8s deployment/myapp --condition Available` | Checks Kubernetes resource conditions or arbitrary fields via JSONPath. |

## CLI Grammar

Global flags must appear before the first backend name. Multiple conditions are
chained with `--` followed by a backend name. At least one non-guard condition
is required. Prefix a condition segment with `guard` to make it a fail-fast
guard instead of a readiness requirement.

```text
waitfor [global-flags] condition [-- condition ...]

condition     := backend [backend-flags] target
guard         := guard condition
exec-condition := exec [exec-flags] -- command [args ...]
```

`exec` uses `--` to separate its own flags from the command being run. After
that separator, tokens belong to the command until a later `-- BACKEND` or
`-- guard BACKEND` condition separator is encountered. This means an exec
command that passes a literal `-- http`, `-- tcp`, `-- dns`, `-- docker`,
`-- exec`, `-- file`, `-- log`, `-- k8s`, or `-- guard BACKEND` token sequence
cannot be followed unambiguously by a second waitfor condition.

A `--` that appears immediately after a value-taking backend flag (such as
`--contains`) is treated as that flag's value, not as a condition separator:

```bash
# Matches lines containing the literal string "--"
waitfor log /var/log/app.log --contains --

# Use a second separator to chain a condition after such a value
waitfor log /var/log/app.log --contains -- -- http https://api.example.com
```

### Global Flags

| Flag | Default | Meaning |
| ---- | ------- | ------- |
| `--timeout duration` | `5m` | Global deadline for the run. |
| `--interval duration` | `2s` | Delay between poll attempts. |
| `--attempt-timeout duration` | `0` (disabled) | Per-attempt deadline; `0` means each attempt receives the remaining global time. |
| `--successes N` | `1` | Consecutive successful checks required before a non-guard condition is complete. |
| `--stable-for duration` | `0` (disabled) | Required continuous success duration before a non-guard condition is complete. |
| `--output text\|json` | `text` | Output format. JSON is written to stdout; text progress goes to stderr. |
| `--mode all\|any` | `all` | `all` requires every condition to satisfy; `any` stops after the first. |
| `--verbose` | `false` | Emit a line per attempt in text mode. |

### Backend Grammar

```text
http URL
     [--status CODE|CLASS]          # e.g. 200 or 2xx (default: 200)
     [--method METHOD]              # default: GET
     [--header KEY=VALUE ...]       # KEY: VALUE also accepted; repeatable
     [--body TEXT | --body-file PATH]
     [--body-contains TEXT]
     [--body-matches REGEX]
     [--jsonpath EXPR]
     [--insecure]
     [--no-follow-redirects]

tcp HOST:PORT

dns HOST
    [--type TYPE]                   # A|AAAA|CNAME|TXT|ANY|MX|SRV|NS|CAA|HTTPS|SVCB (default: A)
    [--resolver system|wire]        # default: system
    [--contains TEXT]
    [--equals VALUE ...]            # repeatable
    [--min-count N]
    [--absent]
    [--absent-mode any|nxdomain|nodata]
    [--server ADDRESS]              # required for --resolver wire; port defaults to 53
    [--rcode CODE]                  # e.g. NOERROR, NXDOMAIN, SERVFAIL
    [--transport udp|tcp]           # default: udp
    [--edns0]
    [--udp-size N]

docker CONTAINER
       [--status STATUS]            # any|created|running|paused|restarting|removing|exited|dead (default: running)
       [--health HEALTH]            # healthy|unhealthy|starting|none (default: disabled)

exec [--exit-code N]               # default: 0
     [--output-contains TEXT]
     [--jsonpath EXPR]
     [--cwd PATH]
     [--env KEY=VALUE ...]          # repeatable
     [--max-output-bytes N]         # default: 1 MiB
     -- COMMAND [ARGS...]

file PATH
     [--exists]                     # default when no state flag is given
     [--deleted]
     [--nonempty]
     [--contains TEXT]              # substring match on file content; first 10 MiB only

log PATH
    (--contains TEXT | --matches REGEX | --jsonpath EXPR)  # at least one required
    [--contains TEXT]
    [--matches REGEX]
    [--exclude REGEX]               # drop lines matching this before applying other matchers
    [--jsonpath EXPR]
    [--from-start]                  # scan from byte 0; default skips existing content
    [--tail N]                      # scan last N lines of existing content before tailing
    [--min-matches N]               # cumulative matching lines required (default: 1)

k8s RESOURCE                        # kind/name, or kind when --selector is used
    [--condition TYPE]              # checks .status.conditions[] for type with status=True
    [--for ready|rollout|complete]  # typed waits for pods, workloads, and jobs
    [--selector LABELS]             # list kind-level resources by label selector
    [--all]                         # with --selector, require every selected resource
    [--jsonpath EXPR]               # mutually exclusive with --condition and --for
    [--namespace NAMESPACE]         # default: default
    [--kubeconfig PATH]
```

## Backend Contracts

### HTTP

One request is made per check. Network errors, non-matching statuses, and body
matcher failures are retryable. Invalid URLs, invalid status matchers, invalid
headers, invalid body regexes, unreadable body files, and invalid JSON
expressions are argument errors caught at parse time.

Runtime JSON parse or expression evaluation failures are retryable because a
service may emit non-JSON output before becoming ready. `--body` and
`--body-file` are mutually exclusive. Redirects are followed by default;
TLS verification is enabled by default.

Status matchers accept an exact code (`200`, `404`) or a class (`2xx`, `5xx`).

### TCP

`HOST:PORT` format is validated at parse time. Each check opens a TCP
connection with the attempt context and closes it immediately on success.
Dial failures are retryable.

### DNS

Validated at parse time: labels must be 63 bytes or fewer; the full name must
fit within the DNS wire limit. Labels containing invalid characters are
rejected. Validation failures are fatal when the condition is constructed
directly and argument errors through the CLI.

Supported record types: `A`, `AAAA`, `CNAME`, `TXT`, `ANY`, `MX`, `SRV`,
`NS`, `CAA`, `HTTPS`, `SVCB`. The default is `A`.

**Resolver modes:**

| Mode | Behavior |
| ---- | -------- |
| `system` | Default. Go's standard resolver. Supports `A`, `AAAA`, `CNAME`, `TXT`, `ANY`. Absence detection treats resolver "not found" errors as absent, but does not distinguish NXDOMAIN from NODATA. |
| `wire` | `codeberg.org/miekg/dns` v2. Requires `--server`. Supports all listed types, exact RCODE checks, NXDOMAIN vs NODATA absence, UDP/TCP transport, EDNS0, and payload size tuning. |

**Matchers:**

| Option | Meaning |
| ------ | ------- |
| `--contains TEXT` | At least one answer string must contain `TEXT`. |
| `--equals VALUE` | Required exact answer. Repeatable; all requested values must be present. CNAME and NS equality ignores case and a single trailing dot. |
| `--min-count N` | At least `N` answers must be present. |
| `--absent` | Wait until the name or record is absent. Cannot combine with `--contains`, `--equals`, or `--min-count`. |
| `--rcode CODE` | Wire-only. Response code must match; satisfies alone if no other positive matcher is set. |

`--absent-mode any` (default) accepts NXDOMAIN, NODATA, zero answers, or a
resolver "not found" error. `nxdomain` and `nodata` require `--resolver wire`.

`--server` accepts: hostname, IPv4, `[IPv6]`, bare IPv6, or `host:port`; port
defaults to `53`. If a UDP response is truncated the wire resolver retries over
TCP. `--udp-size` must be between `0` and `65535`.

Wire answers are filtered to the requested type before value matching. `ANY`
exposes all answers. CNAME records in an A/AAAA response do not satisfy A/AAAA
checks unless a matching address is also present.

Canonical answer value formats:

| Type | Format |
| ---- | ------ |
| `A`, `AAAA` | IP string, e.g. `192.0.2.1` or `2001:db8::1` |
| `CNAME`, `NS` | DNS name with trailing dot; equality ignores case and one trailing dot |
| `TXT` | TXT chunks joined without separators |
| `MX` | `preference exchange`, e.g. `10 mail.example.test.` |
| `SRV` | `priority weight port target`, e.g. `1 2 443 svc.example.test.` |
| `CAA` | `flag tag value`, e.g. `0 issue letsencrypt.org` |
| `HTTPS`, `SVCB` | Library SVCB string, e.g. `1 svc.example.test. alpn="h2"` |

### Docker

Shells out to:

```text
docker inspect --type container --format "{{json .State}}" CONTAINER
```

A missing Docker binary is fatal. Missing containers, daemon connection
failures, inspect errors, and non-matching state or health are retryable.

Valid statuses: `any`, `created`, `running`, `paused`, `restarting`,
`removing`, `exited`, `dead`. Default is `running`; `any` disables the status
check. Valid health values: `healthy`, `unhealthy`, `starting`, `none`.
Empty string (the default) disables the health check; `none` means the
container has no health configuration.

### Exec

Starts the command with the attempt context. A missing binary or spawn failure
is fatal. Non-matching exit codes, output substring failures, and JSON
expression failures are retryable.

On Unix, commands run in a separate process group so cancellation propagates
to shell descendants. `--exit-code` must be non-negative. `stdout` and
`stderr` are merged for `--output-contains`; `--jsonpath` evaluates `stdout`
only. `--max-output-bytes` caps capture (default: 1 MiB). `--env` entries must
use `KEY=VALUE` form.

### File

Checks local filesystem state against one of three mutually exclusive state
flags (default `--exists`):

| Flag | Satisfied when |
| ---- | -------------- |
| `--exists` | Path exists (any type). |
| `--deleted` | Path does not exist. |
| `--nonempty` | Path exists and has size greater than zero. |

`--contains TEXT` reads the file and requires a substring match within the
first 10 MiB. It is valid only with `--exists` or `--nonempty`; combining it
with `--deleted` is invalid. Content checks on non-regular files (directories,
devices) are fatal. Missing paths, empty files, and substring mismatches are
retryable.

### Log

Tails a file and returns satisfied when enough matching lines have appeared.

**Offset tracking.** On the first check with an existing file, the byte offset
is initialised: `--from-start` sets it to zero; `--tail N` scans up to 1 MiB
from the end to find the start of the last N lines; the default sets it to the
current file size (tail-only, skips all existing content). If the file is
missing when waiting starts, the first file created at that path is scanned from
the beginning. Subsequent checks read up to 10 MiB of new content from the saved
offset and advance it to the end of what was read.

**Rotation detection.** Each check calls `os.Stat` and compares the result
with the previously seen `os.FileInfo` using `os.SameFile`. If the inode has
changed, or if the file size shrinks below the saved offset, the offset and
cumulative match count are reset to zero and the new content is scanned from
the beginning.

**Line matching (AND semantics).** For each new line:

1. If `--exclude REGEX` is set and the line matches, the line is dropped.
2. If `--contains TEXT` is set and the line does not contain `TEXT`, the line
   is skipped.
3. If `--matches REGEX` is set and the line does not match, the line is
   skipped.
4. If `--jsonpath EXPR` is set, the line is parsed as JSON; if parsing fails
   or the expression is false, the line is skipped.
5. A line that passes all configured checks increments the cumulative match
   count.

At least one of `--contains`, `--matches`, or `--jsonpath` must be provided.
They may be combined freely; all must pass (AND). `--exclude` may be combined
with any of them.

**Satisfaction.** The condition is satisfied when the cumulative match count
reaches `--min-matches` (default: `1`). The `Result.Detail` field of a
satisfied result contains the matched line, truncated to 200 characters with
a `...` suffix if longer. When `--min-matches` is greater than one, the detail
is prefixed with the count: `"3 matches; last: <line>"`. Unsatisfied results
report progress when at least one match has been counted: `"1 of 3 required
matches"`.

**Constraints.** `--from-start` and `--tail` are mutually exclusive.
`--min-matches` must be at least `1`. A missing file is retryable (the service
may not have written its log yet).

### Kubernetes

Resources use `kind/name` syntax, or plain `kind` syntax with `--selector`.
Supported kinds: `pod`, `service`, `deployment`, `replicaset`, `statefulset`,
`daemonset`, `job`, `namespace`. All are namespaced except `namespace`. The
default namespace is `default`.

Uses the client-go dynamic client in production; a `KubernetesGetter`
interface in tests. API lookup errors and missing resources are retryable.
Unsupported kinds and malformed resource strings are fatal when constructed
directly and argument errors through the CLI.

`--condition TYPE` checks `.status.conditions[]` for a condition whose `type`
matches and whose `status` is `"True"`. `--jsonpath EXPR` evaluates the
minimal expression language against the full unstructured object. `--for`
enables typed waits:

| `--for` value | Supported resources | Satisfied when |
| ------------- | ------------------- | -------------- |
| `ready` | `pod` | Ready condition is `True`; phase `Failed` is fatal. |
| `rollout` | `deployment`, `statefulset`, `daemonset` | Observed generation has caught up and updated/ready or available counts meet desired counts. |
| `complete` | `job` | Complete condition is `True`; Failed condition is fatal. |

`--selector LABELS` lists resources by kind and applies the typed wait to the
matched objects. With `--all`, every selected object must satisfy the typed
wait; without it, the first satisfied object completes the condition. An empty
selector result is retryable. `--condition`, `--jsonpath`, and `--for` are
mutually exclusive.

## Core Contract

Every backend implements:

```go
type Condition interface {
    Descriptor() Descriptor
    Check(ctx context.Context) Result
}

type Wrapper interface {
    UnwrapCondition() Condition
}

type Result struct {
    Status CheckStatus  // CheckSatisfied | CheckUnsatisfied | CheckFatal
    Detail string       // human-readable summary; included in JSON output
    Err    error        // last observed error; included in JSON as last_error
}
```

`Check` must be safe to call repeatedly from a single goroutine and must return
promptly when `ctx` is cancelled. Retryable failures return `CheckUnsatisfied`;
unrecoverable failures return `CheckFatal`. Stateful backends (e.g. `log`) may
store mutable fields directly on the struct because the runner serializes
`Check` calls for the same pointer condition instance. Wrappers such as guards
and names implement `Wrapper`, so two wrappers around the same inner pointer are
serialized against that shared inner condition.

## Runner

The runner owns all polling behaviour:

```go
type Config struct {
    Conditions        []condition.Condition
    Timeout           time.Duration
    Interval          time.Duration
    PerAttemptTimeout time.Duration
    RequiredSuccesses int
    StableFor         time.Duration
    Mode              Mode          // ModeAll | ModeAny
    OnAttempt         func(AttemptEvent) // synchronous and serialized
}
```

Conditions are polled concurrently, one goroutine per condition. `ModeAll`
waits for every condition to satisfy; `ModeAny` cancels remaining work after
the first satisfaction. The first terminal result wins, so a fatal guard that
completes after all required readiness conditions have already completed does
not turn a satisfied run into a fatal run. A per-attempt timeout of `0` passes the
global run context directly to each check. If `PerAttemptTimeout` exceeds the
global `Timeout`, it is normalised to the global timeout before the run starts.

`OnAttempt` is called after each recorded backend `Check` call. The runner
serializes callbacks and waits for them before returning, so progress lines
cannot be written after the final summary. Slow callbacks can delay polling.
The JSON `attempts` field counts recorded backend `Check` calls, not cancelled
waits for a shared condition gate or late terminal checks ignored after the run
has already completed.

`RequiredSuccesses` and `StableFor` apply only to non-guard conditions. A
successful backend check is treated as still pending until the configured
consecutive success count and continuous stable duration are both met. Any
unsatisfied check resets the stability streak.

Guard conditions are polled concurrently but are ignored for satisfaction. If a
guard condition becomes satisfied, it is converted to a fatal result and the
run stops immediately. Once all non-guard conditions have completed in
`ModeAll`, the runner cancels remaining guards and returns satisfied.

## Output

Human-readable progress and summaries are written to **stderr**. JSON output
is written to **stdout** with no progress lines, making it safe to consume in
scripts.

JSON schema (stable):

```json
{
  "status": "satisfied",
  "satisfied": true,
  "mode": "all",
  "elapsed_seconds": 1.2,
  "timeout_seconds": 300.0,
  "interval_seconds": 2.0,
  "per_attempt_timeout_seconds": 5.0,
  "required_successes": 3,
  "stable_for_seconds": 10.0,
  "conditions": [
    {
      "backend": "log",
      "target": "/var/log/app.log",
      "name": "log /var/log/app.log",
      "satisfied": true,
      "attempts": 3,
      "elapsed_seconds": 1.0,
      "detail": "matched: service ready at port 8080",
      "last_error": "",
      "fatal": false,
      "guard": false
    }
  ]
}
```

`per_attempt_timeout_seconds` is omitted when the per-attempt timeout is zero.
`required_successes` is omitted when it is `1`; `stable_for_seconds` is omitted
when the stable duration is zero. `guard` is omitted or false for normal
readiness conditions.
`last_error` is omitted or empty when no error was recorded. Text summaries on
failure list each unsatisfied condition with its last error when available,
otherwise its last detail.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| `0` | All (or any) conditions satisfied |
| `1` | Timeout expired |
| `2` | Invalid arguments or configuration |
| `3` | Unrecoverable condition failure |
| `130` | Cancelled by SIGINT or parent context |
| `143` | Cancelled by SIGTERM |

## Growth Rules

1. **One file per backend.** `internal/condition/<name>.go` implementing
   `condition.Condition`; parser wiring in `internal/cli/cli.go`; unit tests in
   `internal/condition/<name>_test.go`; e2e coverage in `e2e/e2e_test.go` for
   the satisfied, timeout, invalid-args, and fatal paths.

2. **Cyclomatic complexity ≤ 9.** Every production function must stay at or
   below gocyclo score 9. Extract package-level helpers for validation and
   response classification rather than growing a single function.

3. **No polling in backends.** Backends must not own loops, sleepers, output
   formatting, or `os.Exit` calls. All of that belongs in the runner and CLI.

4. **Testability without real services.** Unit tests must work without real
   external services. Use fakes, injected interfaces, local `httptest` servers,
   or `t.TempDir()` files. Real binary and cluster coverage belongs in
   `integration/blackbox_test.go` behind opt-in environment variables
   (`WAITFOR_BLACKBOX=1`, `WAITFOR_BLACKBOX_DOCKER=1`, `WAITFOR_BLACKBOX_K8S=1`).

5. **No new dependencies without a concrete gap.** Use stdlib or existing deps
   first. A new module needs a capability that cannot be covered otherwise (e.g.
   DNS wire-level checks required `codeberg.org/miekg/dns`).

6. **Security lint stays on.** `gosec` runs as a separate CI step. Suppress
   individual findings with `// #nosec GXXX -- <reason>` and a justification;
   never disable the linter globally.

7. **CI gates.** Coverage must stay at or above 90% total. The lint and gosec
   passes must both be clean before merge.
