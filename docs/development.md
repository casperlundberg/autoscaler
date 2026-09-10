# Working on this service

## Getting started

```bash
make test          # unit tests
make check         # what CI runs: gofmt, vet, race tests
make run           # serve on :8080
```

There is nothing to install and nothing to stand up. Every test runs against a
real protocol server — a fake Kubernetes API, a fake ColonyOS server that
verifies signatures, a fake Docker daemon on a real unix socket — so the suite
needs no cluster, no daemon and no network.

## Trying it by hand

```bash
export TOKEN=dev-token
AUTOSCALER_API_TOKEN=$TOKEN make run &

# What can this build scale, and what does each platform need?
curl -sH "Authorization: Bearer $TOKEN" localhost:8080/v1/platforms | jq

# Register a simulation target: no credentials, nothing created.
curl -sH "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"target":{"id":"storhall","name":"Storhall","kind":"simulation","mode":"driven",
       "config":{"local_coldstart_seconds":"0"}},
       "settings":{"local_executor_cap":10,"cloud_executor_cap":20}}' \
  localhost:8080/v1/targets | jq

# Hand it a queue and see what the engine decides.
curl -sH "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"at":"2026-09-10T12:00:00Z",
       "workload":{"queues":{"100":{"depth":400,"oldest_job_age_seconds":50,
                   "arrival_rate_per_second":2}},
                   "executor_throughput_per_second":1}}' \
  localhost:8080/v1/targets/storhall/cycle | jq '.decision | {action, plan, reason}'

# Change the policy, and watch the next cycle obey it immediately.
curl -sX PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"local_executor_cap": 2, "cloud_executor_cap": 0}' \
  localhost:8080/v1/targets/storhall/settings | jq '{version, warnings}'
```

## Where things live

| Package | What it owns |
|---|---|
| `internal/domain` | The vocabulary: observations, plans, decisions. Imports nothing of ours. |
| `internal/config` | The settings document, its validation, and the store that swaps versions at runtime. |
| `internal/policy` | The decision engine. Pure functions; no I/O, no clock, no logging. |
| `internal/platform` | The `Provisioner` interface, target definitions, and the adapter registry. |
| `internal/platform/*` | One package per platform. None of them import `policy`. |
| `internal/secret` | Credentials that redact when serialised. |
| `internal/registry` | Registered targets, their settings stores, their last cycle, and persistence. |
| `internal/controller` | The loop, and the runner that ticks autonomous targets. |
| `internal/api` | HTTP. Parse, delegate, render. |
| `internal/app` | Wiring and process lifecycle. |

Dependencies point downward only. If you find yourself wanting `policy` to
import a platform, or a platform to import `policy`, the thing you are
building belongs in `controller`.

## Adding a platform

Everything a new platform needs is behind one interface, and nothing above it
has to change.

1. Create `internal/platform/<name>/`.
2. Implement `Kind`, `Schema`, `Validate`, `Observe`, `Apply`.
3. Register it in `internal/app.Platforms()`.

The `Schema` is not documentation — it is the API. The UI renders a
registration form from it, `Check` validates targets against it, and defaults
are read from it. Give every field a `Label` (there is a test that fails
otherwise) and put anything surprising in `Description`, because that string is
what an operator reads at the moment they have to decide what to type.

Set `SeesWorkload` honestly. If the platform cannot report its own queue, say
so: targets on it will be refused autonomous mode, which is correct — polling
a platform that reports only capacity decides from an empty queue every cycle
and takes every tier to its floor.

### Testing a new adapter

Follow what the existing ones do: stand up a server that speaks the real
protocol and test against it. `kubetest` and `colonytest` are there to be
reused. Fakes that behave like the real thing catch request-building, encoding
and error-handling bugs; mocks of your own client catch none of them.

If the protocol involves cryptography, pin it against the real implementation
the way `internal/platform/colonyos/identity_test.go` does. A signature scheme
that is subtly wrong authenticates against nothing and is otherwise discovered
for the first time in production.

## Adding a setting

1. Add the field to `config.Settings`, with a comment saying what it is for.
2. Add it to `settingsWire` with a `_seconds` suffix if it is a duration.
3. Validate it in `Settings.Validate` — and say in the error message what
   goes wrong if it is set that way.
4. Document it in `api/openapi.yaml`.

The change log, the patch semantics and the audit diff all come for free: they
work off the JSON rendering, so there is no second list to keep in step.

## Conventions

- **Tests first.** Every commit in this repository has its tests written
  before its implementation, and the test names are sentences about behaviour
  rather than about method names.
- **Comments say why.** What the code does is visible; what it would have been
  reasonable to do instead, and why that is wrong, is not.
- **Errors name the thing.** A message that says which field, which
  Deployment, which target, and what a valid value looks like is the
  difference between a two-minute fix and an afternoon.
- **Durations are seconds on the wire**, `time.Duration` in Go.
- **No sleeping in tests.** Time is passed in, never read from the clock.
