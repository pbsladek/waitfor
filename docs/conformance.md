# Backend conformance matrix

Every backend should have evidence for these behavior classes:

| Backend | Satisfied | Timeout | Invalid args | Fatal | Cancellation | JSON/NDJSON |
| --- | --- | --- | --- | --- | --- | --- |
| http | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| tcp | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| unix | unit, black-box | black-box | cli tests | unit | runner | json/e2e |
| ports | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| tls | unit, black-box | black-box | cli tests | unit | runner | json/e2e |
| ssh | unit, black-box | black-box | cli tests | unit | runner | json/e2e |
| s3 | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| dns | unit, black-box | e2e | cli tests | unit | runner | json/e2e |
| docker | unit, optional black-box | optional black-box | cli tests | unit | runner | json/e2e |
| process | unit, black-box | black-box | cli tests | unit | runner | json/e2e |
| systemd | unit, e2e | e2e | cli tests | unit | runner | json/e2e |
| launchd | unit, e2e, black-box fake | e2e | cli tests | e2e | runner | json/e2e |
| pidfile | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| lockfile | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| permission | unit, e2e, black-box | e2e | cli tests | e2e | runner | json/e2e |
| checksum | unit, e2e, black-box | e2e | cli tests | e2e | runner | json/e2e |
| archive | unit, e2e, black-box | e2e | cli tests | e2e | runner | json/e2e |
| cosign | unit, e2e, black-box fake | e2e | cli tests | e2e | runner | json/e2e |
| ntp | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| icmp | unit, e2e, black-box fake | e2e | cli tests | e2e | runner | json/e2e |
| grpc | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| websocket | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| exec | unit, e2e, black-box | e2e | cli tests | e2e | runner | json/e2e |
| file | unit, e2e, black-box | black-box | cli tests | unit | runner | json/e2e |
| glob | unit, e2e, black-box | e2e | cli tests | unit | runner | json/e2e |
| log | unit, e2e | e2e | cli tests | unit | runner | json/e2e |
| k8s | unit, optional black-box | optional black-box | cli tests | unit | runner | json/e2e |

When a backend is added or expanded, update this matrix and add tests for any
cell that would otherwise rely only on parser coverage.
