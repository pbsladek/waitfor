# waitfor

[![CI](https://github.com/pbsladek/wait-for/actions/workflows/ci.yml/badge.svg)](https://github.com/pbsladek/wait-for/actions/workflows/ci.yml)
[![Docker](https://github.com/pbsladek/wait-for/actions/workflows/docker.yml/badge.svg)](https://github.com/pbsladek/wait-for/actions/workflows/docker.yml)
[![Release](https://github.com/pbsladek/wait-for/actions/workflows/release.yml/badge.svg)](https://github.com/pbsladek/wait-for/actions/workflows/release.yml)

## What

`waitfor` blocks until something is actually ready, then exits `0`. Use it in
shell scripts, CI pipelines, Docker entrypoints, Kubernetes init containers, and
agent workflows when a plain sleep is too brittle.

It can wait for HTTP health checks, TCP ports, DNS records, Docker containers,
commands, files, log lines, and Kubernetes resources. If the timeout expires,
it exits `1`; invalid input exits `2`; unrecoverable condition failures exit
`3`.

Human-readable progress is written to stderr. JSON output is written to stdout
without progress lines so it is safe to consume from scripts.

Supported waits:

```text
http, tcp, dns, docker, exec, file, log, k8s
```

## Install

Download prebuilt binaries from the
[GitHub Releases](https://github.com/pbsladek/wait-for/releases) page.

Install the latest Linux x86_64 release:

```bash
curl -fsSLO https://github.com/pbsladek/wait-for/releases/latest/download/waitfor_linux_amd64.tar.gz
tar -xzf waitfor_linux_amd64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

Install a specific Linux x86_64 release:

```bash
VERSION=v0.8.0
curl -fsSLO "https://github.com/pbsladek/wait-for/releases/download/${VERSION}/waitfor_linux_amd64.tar.gz"
tar -xzf waitfor_linux_amd64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

Install the latest macOS Apple Silicon release:

```bash
curl -fsSLO https://github.com/pbsladek/wait-for/releases/latest/download/waitfor_darwin_arm64.tar.gz
tar -xzf waitfor_darwin_arm64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

Install a specific macOS Apple Silicon release:

```bash
VERSION=v0.8.0
curl -fsSLO "https://github.com/pbsladek/wait-for/releases/download/${VERSION}/waitfor_darwin_arm64.tar.gz"
tar -xzf waitfor_darwin_arm64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

Use `waitfor_linux_arm64.tar.gz`, `waitfor_darwin_amd64.tar.gz`,
`waitfor_windows_amd64.zip`, or `waitfor_windows_arm64.zip` for other
platforms.

Install from source with Go:

```bash
go install github.com/pbsladek/wait-for/cmd/waitfor@latest
```

Build from a checkout:

```bash
make build
bin/waitfor --help
```

Run with Docker:

```bash
docker pull pwbsladek/waitfor:latest
docker run --rm pwbsladek/waitfor:latest --help
```

Tagged images are published as `pwbsladek/waitfor:<tag>`.

## Usage

```text
waitfor [flags] <backend> <target> [backend-flags]
waitfor [flags] <backend> ... -- <backend> ...
```

Global flags:

```text
--timeout duration     Global deadline (default: 5m)
--interval duration    Poll interval (default: 2s)
--backoff constant|exponential
                       Poll backoff strategy (default: constant)
--max-interval duration
                       Maximum poll interval for exponential backoff (default: --interval)
--jitter percent       Poll interval jitter, for example 20% or 0.2 (default: 0%)
--attempt-timeout duration
                       Per-attempt deadline; 0 disables per-attempt limit
--successes int        Consecutive successful checks required (default: 1)
--stable-for duration  Required continuous success duration (default: disabled)
--output text|json     Output format (default: text)
--mode all|any         Condition mode (default: all)
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
log PATH (--contains text | --matches regex | --jsonpath expr) [--exclude regex] [--from-start|--tail N] [--min-matches N]
k8s RESOURCE [--condition Ready] [--for ready|rollout|complete] [--selector labels] [--all] [--namespace default] [--jsonpath expr] [--kubeconfig path]
```

Every condition accepts `--name LABEL` for human-readable text progress and JSON
summaries. For guards, the label is prefixed with `guard` in output.

By default, all conditions must pass. Use `--mode any` when the first satisfied
condition should complete the run.

## Examples

Wait for HTTP readiness:

```bash
waitfor http https://api.example.com/health --status 200

waitfor http https://api.example.com/health --status 200 --body-contains ok

waitfor http https://api.example.com/ready --jsonpath '.ready == true'
```

Wait for ports and DNS:

```bash
waitfor tcp localhost:5432

waitfor dns api.example.com --type A --min-count 1

waitfor dns api.example.com --resolver wire --server 1.1.1.1 --type HTTPS --rcode NOERROR
```

Wait for local process state:

```bash
waitfor docker my-container --status running --health healthy

waitfor file /tmp/ready.flag --exists

waitfor file /tmp/lock --deleted

waitfor exec --output-contains Running -- kubectl get pod myapp
```

Wait for log output:

```bash
waitfor log /var/log/app.log --contains "server ready"

waitfor log /var/log/app.log --matches "ERROR:.*timeout" --from-start

waitfor log /var/log/app.log --contains ready --tail 100 --min-matches 2
```

Wait for Kubernetes resources:

```bash
waitfor k8s deployment/myapp --condition Available --namespace prod

waitfor k8s deployment/myapp --for rollout --namespace prod

waitfor k8s pod --selector app=myapp --for ready --all --namespace prod
```

Use timing controls for noisy services:

```bash
waitfor --successes 3 http https://api.example.com/health --status 200

waitfor --stable-for 30s http https://api.example.com/health --status 200

waitfor --backoff exponential --max-interval 5s --jitter 20% http https://api.example.com/health --name api
```

Use JSON output in scripts:

```bash
waitfor --output json http https://api.example.com/health --status 200

waitfor --output json --mode any \
  http https://primary.example.com/health \
  -- http https://fallback.example.com/health
```

Multiple conditions are chained with `--` before the next backend:

```bash
waitfor --timeout 10m \
  http https://api.example.com/health \
  -- tcp localhost:5432 \
  -- k8s deployment/myapp --condition Available
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

Prefix a condition with `guard` to fail fast if that condition becomes true
while the main readiness conditions are still pending:

```bash
waitfor http https://api.example.com/health \
  -- guard log /var/log/app.log --matches 'FATAL|panic'
```

## Backend Details

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

Kubernetes typed waits cover common rollout gates without custom JSON
expressions: `--for rollout` for deployments, statefulsets, and daemonsets;
`--for ready` for pods; and `--for complete` for jobs. `--selector` switches
from `kind/name` to kind-level list mode, with `--all` requiring every selected
object to satisfy the typed wait.

## Environment Checks

`waitfor doctor` reports local support for optional integrations and scripting
environment assumptions. Docker and Kubernetes are warnings by default because
those backends are optional; add `--require docker,k8s` when a pipeline must fail
if either integration is unavailable. `--require` is repeatable and also accepts
comma-separated values.

```bash
waitfor doctor --output json

waitfor doctor --require docker,k8s --output json
```

```text
waitfor doctor [--output text|json] [--require temp|shell|docker|k8s|dns-wire]
```

## JSON Expressions

JSON expressions intentionally use a small, predictable subset:

```text
.field
.field.subfield == "value"
.field >= 10
.items[0].name == "first"
{.status.phase}=Running
```

Expressions can be used with HTTP response bodies, command output, log lines,
and Kubernetes resources.

## Exit Codes

| Code | Meaning |
| ---- | ------- |
| 0 | Conditions satisfied |
| 1 | Timeout expired before conditions were met |
| 2 | Invalid arguments or configuration |
| 3 | Unrecoverable condition failure |
| 130 | Cancelled by parent context or SIGINT |
| 143 | Cancelled by SIGTERM |

## Docker Image

The image is built from Docker Hardened Images: `dhi.io/golang:1.26-dev` for
compilation and `dhi.io/static:20250419` for the distroless runtime.

Local image builds require access to DHI:

```bash
docker login dhi.io
make docker-build
make docker-run ARGS="http https://api.example.com/health --status 200"
```

Publishing a multi-arch image:

```bash
docker login
docker login dhi.io
make docker-push DOCKER_TAG=tagname
```

## Scripting Notes

Use text output for humans and JSON output for automation. Text progress is
written to stderr. JSON summaries are written to stdout and omit progress lines,
so scripts can safely pipe or parse stdout.

Condition records include `backend`, `target`, and `name`. Prefer `backend` and
`target` for automation; `name` is a human-readable label and may come from
`--name`.

Timeouts and retryable failures exit `1`. Fatal configuration or environment
errors exit `3`, for example a missing command binary for `exec` or an invalid
Kubernetes configuration.

## Maintainers

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

### Releases

Create a GitHub Release by pushing a version tag:

```bash
make release-tag VERSION=v0.1.0
```

The `Release` workflow runs automatically when a `v*` tag is pushed. It checks
out the exact tag, runs the Go test suite and black-box binary integration
tests, then runs GoReleaser to publish release notes, archives, and checksums to
GitHub.

The tag must point at a commit that already contains the release workflow.
`make release-tag` checks this before creating the tag.

If a tag was pushed before this workflow existed, GitHub cannot replay that tag
creation event. Create the release from the command line instead of using the UI:

```bash
make release-existing VERSION=v0.1.0
```

For a local artifact-only dry run:

```bash
make release-snapshot
```
