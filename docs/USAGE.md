# waitfor Usage Guide

A collection of real-world examples showing waitfor on its own, combined with
other CLI tools, and embedded in scripts. Every example shows the command,
what you would see on the terminal, and how to use the result.

---

## 1. Wait for an HTTP service to become healthy

```bash
waitfor http https://api.example.com/health --status 200
```

```
checking 1 condition (timeout 5m, interval 2s)
[ok] http https://api.example.com/health — status 200
conditions satisfied in 4.1s
```

The process exits `0`. Chain the next step directly:

```bash
waitfor http https://api.example.com/health --status 200 && ./run-migrations.sh
```

---

## 2. Require a specific JSON field in the health response

```bash
waitfor --timeout 2m \
  http https://api.example.com/health \
  --jsonpath '.status == "ok"'
```

```
checking 1 condition (timeout 2m, interval 2s)
[..] http https://api.example.com/health — jsonpath condition not satisfied
[ok] http https://api.example.com/health — .status == "ok"
conditions satisfied in 6.2s
```

Useful when a service starts accepting connections before it has finished
initialising its dependency graph.

---

## 3. Wait for a TCP port and pipe success into a notification

```bash
waitfor tcp postgres:5432 && \
  notify-send "waitfor" "Database port is open"
```

`notify-send` only fires when `waitfor` exits `0`. On a server without a
desktop environment, swap in a Slack webhook:

```bash
waitfor tcp postgres:5432 && \
  curl -s -X POST "$SLACK_WEBHOOK" \
       -H 'Content-Type: application/json' \
       -d '{"text":"postgres port open — starting app"}'
```

---

## 4. Parse JSON output with jq

```bash
result=$(waitfor --output json \
  http https://api.example.com/health --status 200)

echo "$result" | jq '{status, elapsed: .elapsed_seconds}'
```

```json
{
  "status": "satisfied",
  "elapsed": 3.84
}
```

Extract whether each condition passed individually:

```bash
echo "$result" | jq '.conditions[] | {name, satisfied, attempts}'
```

```json
{
  "name": "http https://api.example.com/health",
  "satisfied": true,
  "attempts": 2
}
```

---

## 5. Multi-condition gate before a deployment

Wait for the database, cache, and message broker before starting the app:

```bash
waitfor --timeout 5m --interval 3s \
  tcp postgres:5432 \
  -- tcp redis:6379 \
  -- tcp rabbitmq:5672 \
&& docker compose up -d app
```

All three must pass. If any times out, `docker compose up` does not run.

---

## 6. Use `--mode any` for a fallback readiness signal

Accept either the primary API or its canary as the readiness signal:

```bash
waitfor --mode any --timeout 3m \
  http https://api.example.com/health \
  -- http https://canary.example.com/health
```

```
checking 2 conditions (timeout 3m, interval 2s, mode: any)
[ok] http https://canary.example.com/health — status 200
conditions satisfied in 1.2s (any mode)
```

---

## 7. Capture the timeout exit code in a script

```bash
if waitfor --timeout 30s tcp localhost:5432; then
  echo "database ready"
else
  code=$?
  if [ "$code" -eq 1 ]; then
    echo "timed out waiting for database" >&2
  elif [ "$code" -eq 3 ]; then
    echo "fatal error — check docker logs" >&2
  fi
  exit "$code"
fi
```

---

## 8. Wait for a file to appear then read it

```bash
waitfor file /run/secrets/api-key --exists && \
  API_KEY=$(cat /run/secrets/api-key)
```

Or wait for the file to have content before reading:

```bash
waitfor file /run/app/config.json --nonempty && \
  jq .database_url /run/app/config.json
```

---

## 9. Gate on file content

Wait until a config file is written with the expected environment marker before
starting dependent services:

```bash
waitfor file /etc/app/config.toml --contains 'environment = "production"' && \
  systemctl start app
```

---

## 10. Tail a log until the service announces readiness

Watch only new content (the default — existing lines are skipped):

```bash
waitfor log /var/log/app.log --contains "listening on port"
```

```
checking 1 condition (timeout 5m, interval 2s)
[..] log /var/log/app.log — no matching log line
[ok] log /var/log/app.log — matched: 2024-06-01T12:00:03Z INFO listening on port 8080
conditions satisfied in 6.1s
```

Then pipe the detail into another command via JSON:

```bash
detail=$(waitfor --output json log /var/log/app.log \
  --contains "listening on port" | jq -r '.conditions[0].detail')
echo "service is up: $detail"
```

---

## 11. Scan a log from the beginning for an error pattern

Useful in CI to assert that a service started without fatal errors:

```bash
waitfor --timeout 10s \
  log /var/log/app.log \
  --matches "FATAL|panic" \
  --from-start
if [ $? -eq 0 ]; then
  echo "fatal error detected in log — failing build" >&2
  exit 1
fi
```

Because `waitfor` exits `0` on satisfaction and `1` on timeout, inverting the
check catches the "bad pattern found" case.

---

## 12. Use `--exclude` to ignore noisy log lines

Wait for a "ready" line in a chatty log, but skip health-check pings that
also contain the word "ready":

```bash
waitfor log /var/log/nginx/access.log \
  --contains "ready" \
  --exclude "GET /healthz" \
  --from-start
```

---

## 13. Require N heartbeats before declaring a service stable

Some services emit periodic heartbeats. Require three consecutive appearances
before trusting the service is stable:

```bash
waitfor --timeout 2m \
  log /var/log/app.log \
  --contains "heartbeat ok" \
  --min-matches 3
```

```
checking 1 condition (timeout 2m, interval 2s)
[..] log /var/log/app.log — 1 of 3 required matches
[..] log /var/log/app.log — 2 of 3 required matches
[ok] log /var/log/app.log — 3 matches; last: heartbeat ok at 12:00:09
conditions satisfied in 14.3s
```

---

## 14. Wait for a DNS record to propagate

```bash
waitfor dns api.example.com --type A --min-count 1
```

After a deployment that changes DNS, wait for the new CNAME to appear using
the wire resolver for an exact check:

```bash
waitfor --timeout 30m --interval 30s \
  dns api.example.com \
  --resolver wire \
  --server 8.8.8.8 \
  --type CNAME \
  --equals "lb.example.com."
```

---

## 15. Send an email alert when a log pattern is detected

Combine `waitfor` with `mail` to alert on the first error:

```bash
waitfor --timeout 24h \
  log /var/log/app.log \
  --matches "ERROR|CRITICAL" \
  --from-start && \
  echo "Error detected in app.log — check the server." | \
    mail -s "Alert: app error" ops@example.com
```

Or capture the matched line for the email body:

```bash
detail=$(waitfor --output json \
  log /var/log/app.log --matches "ERROR|CRITICAL" --from-start |
  jq -r '.conditions[0].detail')

printf "Matched line:\n%s\n" "$detail" | \
  mail -s "Alert: app error" ops@example.com
```

---

## 16. Kubernetes init container

Block a pod from starting its main container until the database migration job
completes:

```yaml
initContainers:
  - name: wait-for-migrate
    image: pwbsladek/waitfor:latest
    args:
      - --timeout
      - 10m
      - k8s
      - job/migrate
      - --condition
      - Complete
      - --namespace
      - production
```

Chain database and cache readiness in the same init container:

```yaml
args:
  - --timeout
  - 5m
  - tcp
  - postgres.production.svc.cluster.local:5432
  - --
  - tcp
  - redis.production.svc.cluster.local:6379
```

---

## 17. Wait for a Kubernetes rollout to finish

```bash
waitfor --timeout 10m \
  k8s deployment/api \
  --condition Available \
  --namespace production && \
  echo "rollout complete"
```

Or watch a field directly instead of a condition:

```bash
waitfor k8s deployment/api \
  --jsonpath '.status.readyReplicas >= 3' \
  --namespace production
```

---

## 18. Docker Compose startup gate

Wait for every service to be healthy before running smoke tests:

```bash
docker compose up -d

waitfor --timeout 3m --interval 2s \
  docker db --status running --health healthy \
  -- docker cache --status running \
  -- http http://localhost:8080/health --status 200 \
&& ./smoke-tests.sh
```

---

## 19. Run a command repeatedly until it succeeds

Wait until `kubectl rollout status` returns exit code `0`:

```bash
waitfor --timeout 10m --interval 5s \
  exec \
  -- kubectl rollout status deployment/api -n production
```

With output inspection — wait until the migration script prints "done":

```bash
waitfor --timeout 5m \
  exec \
  --output-contains "migrations complete" \
  -- psql "$DATABASE_URL" -c "SELECT status FROM schema_migrations ORDER BY id DESC LIMIT 1"
```

---

## 20. Parse final JSON output with jq

In JSON mode `waitfor` writes one final JSON document to stdout. Human progress
stays off stdout, so the result can be piped directly to `jq`:

```bash
waitfor --output json --interval 1s \
  http https://api.example.com/health --status 200 2>/dev/null | \
  jq 'if .status == "satisfied" then "✓ ready after \(.elapsed_seconds)s" else empty end'
```

---

## 21. CI pipeline gate with structured failure reporting

In a CI script, emit JSON on failure so the pipeline can parse which condition
timed out:

```bash
output=$(waitfor --output json --timeout 5m \
  tcp postgres:5432 \
  -- tcp redis:6379 \
  -- http http://api:8080/health)

if [ $? -ne 0 ]; then
  echo "$output" | jq '
    .conditions[]
    | select(.satisfied == false)
    | "FAILED: \(.name) — \(.last_error)"
  ' >&2
  exit 1
fi
```

Example failure output on stderr:

```
"FAILED: tcp redis:6379 — dial tcp redis:6379: connect: connection refused"
```

---

## 22. Wait for a lock file to be released

Block until another process deletes its lock file before proceeding:

```bash
waitfor --timeout 10m file /var/run/deploy.lock --deleted && \
  touch /var/run/deploy.lock && \
  ./deploy.sh; \
  rm -f /var/run/deploy.lock
```

---

## 23. Log-driven deployment promotion

After deploying a canary, wait until the canary log shows no errors in the
first 50 lines of output, using `--tail` to limit the scan window:

```bash
# Deploy canary
kubectl set image deployment/api-canary api=my-image:v2 -n production

# Wait for the canary pod log to be written
waitfor --timeout 2m file /mnt/logs/canary.log --nonempty

# Check the first 50 lines for errors; timeout = "no errors found in window"
if waitfor --timeout 30s log /mnt/logs/canary.log \
     --matches "ERROR|FATAL" \
     --tail 50; then
  echo "errors detected in canary — rolling back" >&2
  kubectl rollout undo deployment/api-canary -n production
  exit 1
fi

echo "canary healthy — promoting to production"
kubectl set image deployment/api api=my-image:v2 -n production
```

---

## 24. Send a Slack message when a long job completes

```bash
waitfor --timeout 6h \
  k8s job/nightly-report \
  --condition Complete \
  --namespace production && \
  curl -s -X POST "$SLACK_WEBHOOK" \
    -H 'Content-Type: application/json' \
    -d '{"text":"✅ nightly-report job finished"}'
```

Capture elapsed time from JSON and include it in the message:

```bash
result=$(waitfor --output json --timeout 6h \
  k8s job/nightly-report --condition Complete \
  --namespace production)

elapsed=$(echo "$result" | jq '.elapsed_seconds | round')
curl -s -X POST "$SLACK_WEBHOOK" \
  -H 'Content-Type: application/json' \
  -d "{\"text\":\"✅ nightly-report finished in ${elapsed}s\"}"
```

---

## 25. Use `--attempt-timeout` for slow health endpoints

Some services take a long time to respond during startup. Set a per-attempt
deadline shorter than the global timeout so a hung request does not burn the
entire budget:

```bash
waitfor \
  --timeout 5m \
  --interval 3s \
  --attempt-timeout 5s \
  http https://api.example.com/health --status 200
```

Each HTTP request gets at most 5 seconds; if it hangs the attempt is cancelled
and retried after the interval. The global 5-minute deadline still applies.

---

## 26. Fail fast when a guard condition appears

Wait for an API, but stop immediately if startup logs show a fatal error:

```bash
waitfor --timeout 5m \
  http https://api.example.com/health \
  -- guard log /var/log/app.log --matches "FATAL|panic"
```

The HTTP condition is the readiness requirement. The log condition is a guard:
if it matches, `waitfor` exits `3` instead of waiting for the full timeout.

---

## 27. Require stable readiness before continuing

Avoid starting the next step on a one-off successful probe:

```bash
waitfor --successes 3 --stable-for 15s \
  http https://api.example.com/health --status 200
```

The service must return the expected response on consecutive checks and remain
successful for the stable duration before the run exits `0`.

---

## 28. Wait for Kubernetes rollouts and selected pods

Use typed waits instead of writing JSON expressions for common Kubernetes
states:

```bash
waitfor k8s deployment/api --for rollout --namespace production
waitfor k8s pod --selector app=api --for ready --all --namespace production
waitfor k8s job/migrate --for complete --namespace production
```

`--for complete` treats a failed job as fatal. `--selector` switches the
resource argument from `kind/name` to plain `kind`.

---

## Tips

**Exit code check in `set -e` scripts.** `waitfor` exits non-zero on timeout or
fatal error, so `set -e` scripts abort naturally without an explicit `if`
block:

```bash
set -e
waitfor tcp postgres:5432
waitfor http http://api:8080/health
./start-app.sh
```

**Combining JSON output with `tee`.** Capture JSON while also showing text
progress on the terminal:

```bash
waitfor --output json http https://api.example.com/health \
  > result.json
# stderr (text progress) still goes to the terminal
# stdout (JSON) goes to result.json
```

**Makefile integration.** Use `waitfor` as a make target dependency:

```makefile
wait-deps:
	waitfor --timeout 2m tcp postgres:5432 -- tcp redis:6379

test: wait-deps
	go test ./...
```
