---
layout: default
title: Home
nav_order: 1
---

# waitfor

Semantic condition poller for shell scripts, CI pipelines, Docker entrypoints,
Kubernetes init containers, and agent workflows. `waitfor` blocks until real
readiness conditions pass, then exits `0`.

**[View on GitHub](https://github.com/pbsladek/wait-for)** · **[Latest release](https://github.com/pbsladek/wait-for/releases/latest)**

---

## Install

### Pre-built binary

Download from the [latest release](https://github.com/pbsladek/wait-for/releases/latest):

| Platform | File |
|---|---|
| Linux x86_64 | `waitfor_linux_amd64.tar.gz` |
| Linux ARM64 | `waitfor_linux_arm64.tar.gz` |
| macOS Apple Silicon | `waitfor_darwin_arm64.tar.gz` |
| macOS Intel | `waitfor_darwin_amd64.tar.gz` |
| Windows x86_64 | `waitfor_windows_amd64.zip` |
| Windows ARM64 | `waitfor_windows_arm64.zip` |

Install the latest Linux x86_64 release:

```bash
curl -fsSLO https://github.com/pbsladek/wait-for/releases/latest/download/waitfor_linux_amd64.tar.gz
tar -xzf waitfor_linux_amd64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

Install a specific release:

```bash
VERSION=v0.9.0
curl -fsSLO "https://github.com/pbsladek/wait-for/releases/download/${VERSION}/waitfor_linux_amd64.tar.gz"
tar -xzf waitfor_linux_amd64.tar.gz waitfor
chmod +x waitfor
sudo mv waitfor /usr/local/bin/waitfor
```

### Go

```bash
go install github.com/pbsladek/wait-for/cmd/waitfor@latest
```

### Docker

```bash
docker pull pwbsladek/waitfor:latest
docker run --rm pwbsladek/waitfor:latest --help
```

---

## Usage

```bash
waitfor [flags] <backend> <target> [backend-flags]
waitfor [flags] <backend> ... -- <backend> ...
```

Supported waits:

```text
http, tcp, unix, tls, ssh, s3, glob, ports, exec, file, log, k8s, dns,
docker, process, systemd, launchd, pidfile, lockfile, permission, checksum,
archive, cosign, ntp, icmp, grpc, websocket
```

Common flags:

```text
--timeout duration      Global deadline (default: 5m)
--interval duration     Poll interval (default: 2s)
--backoff constant|exponential
                        Poll backoff strategy (default: constant)
--max-interval duration Maximum interval for exponential backoff
--jitter percent        Randomize poll intervals, for example 20%
--attempt-timeout duration
                        Per-attempt deadline; 0 disables it
--output text|json|ndjson
                        Output format (default: text)
--mode all|any          Condition mode (default: all)
--verbose               Show every attempt in text mode
--successes N           Consecutive successful checks required
--stable-for duration   Required continuous success duration
--profile ci|local      Apply a defaults profile
--config path           Load a YAML recipe file
--explain, --dry-run    Print the parsed plan without running it
```

---

## Examples

### HTTP

```bash
waitfor http https://api.example.com/health --status 200

waitfor http https://api.example.com/ready --jsonpath '.ready == true'
```

### TCP and DNS

```bash
waitfor tcp localhost:5432

waitfor unix /var/run/docker.sock

waitfor ports localhost --range 8000-8010 --any

waitfor tls api.example.com:443 --valid-for 30d

waitfor ssh host.example.com:22

waitfor ssh host.example.com:22 --user deploy --password "$SSH_PASSWORD"

waitfor s3 s3://bucket/path/ready.json --exists

waitfor s3 s3://bucket/path/ready.json --contains '"ready":true' --endpoint-url http://localhost:9000

waitfor s3 s3://bucket/path/ready.json --endpoint-url https://ceph-rgw.example.com --region default

waitfor dns api.example.com --type A --min-count 1
```

### Files and logs

```bash
waitfor process --name postgres --running

waitfor systemd nginx.service --active

waitfor file /tmp/ready.flag --exists

waitfor glob '/tmp/jobs/*.done' --min-count 5

waitfor log /var/log/app.log --contains "server ready"

waitfor log /var/log/app.log --contains ready --tail 100 --min-matches 2
```

### Local services and artifacts

```bash
waitfor launchd system/com.example.agent --running

waitfor pidfile /var/run/app.pid --running

waitfor lockfile /var/run/app.lock --absent

waitfor permission /etc/app.conf --mode 0640 --gid 1000 --type file

waitfor checksum dist/app.tar.gz --equals "sha256:$SHA256"

waitfor archive dist/app.tar.gz --matches 'bin/*'

waitfor cosign ghcr.io/org/app:v1.2.3 --certificate-identity "$IDENTITY"
```

### Protocol checks

```bash
waitfor ntp time.cloudflare.com --max-offset 250ms

waitfor icmp 192.0.2.10 --count 3 --timeout 1s

waitfor grpc localhost:50051 --service grpc.health.v1.Health

waitfor websocket ws://localhost:8080/events --matches 'ready|ok'
```

### Kubernetes

```bash
waitfor k8s deployment/myapp --for rollout --namespace prod

waitfor k8s pod --selector app=myapp --for ready --all --namespace prod
```

### Multiple conditions

```bash
waitfor --timeout 10m \
  http https://api.example.com/health \
  -- tcp localhost:5432 \
  -- k8s deployment/myapp --condition Available
```

### Machine-readable output and plans

```bash
waitfor --output json http https://api.example.com/health --status 200

waitfor --output ndjson http https://api.example.com/health --status 200

waitfor --config waitfor.yaml --explain
```

---

## More Docs

- [Usage guide](USAGE.html)
- [Implementation spec](SPEC.html)
- [Workflow recipes](workflows.html)
- [Backend conformance matrix](conformance.html)
