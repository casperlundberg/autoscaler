# autoscaler

A scaling controller for priority-aware batch workloads. It decides how many
executors a workload needs, and — unlike a simulator — it can actually go and
create them, on more than one kind of platform.

Three properties drive the design:

1. **Settings are updated at runtime.** Policy numbers (SLA deadlines, executor
   caps, coldstart estimates, hysteresis, cloud burst limits) are changed
   through the API and take effect on the next decision cycle. No restart, no
   redeploy, no config file remount.
2. **The platform is pluggable.** The same decision engine drives plain
   Kubernetes Deployments, ColonyOS executors running as pods, ColonyOS
   executors running as standalone containers, or a simulation adapter that
   reports the decision back instead of enacting it.
3. **It provisions autonomously.** A target is registered with whatever access
   keys that platform needs — a Kubernetes bearer token and CA, a ColonyOS
   executor private key, a Docker daemon endpoint and client certificates,
   registry credentials — and from then on the controller can add and remove
   replicas on its own.

## Platforms

| Kind | What an executor is | Sees its own queue |
|---|---|---|
| `kubernetes` | A pod in a Deployment this service scales | No — supply the queue with each decision |
| `colonyos-k8s` | A ColonyOS executor pod, in a Deployment this service creates | Yes, from the ColonyOS server |
| `colonyos-container` | A ColonyOS executor container on a Docker host, no orchestrator | Yes, from the ColonyOS server |
| `simulation` | A row in a fleet that is aged but never created | No — the queue is the log being replayed |

`GET /v1/platforms` describes every configuration and credential field each one
needs, well enough for a client to render a registration form for a platform it
was never written for.

## How it decides

Every decision comes from simulating the queue forward. A threshold rule
cannot answer the question an SLA asks — *will anything miss its deadline* —
because that depends on arrival rate, on how work is spread across priority
levels, and on how much of it is already old. So the engine plays the queue
forward under a candidate executor count and looks, then searches for the
smallest count with no predicted breach, fills the local tier to its cap, and
sends only the proven overflow to the cloud.

Each decision carries the reasoning that produced it:

> `P100 breaches in 15s at the current 0 executors; 6 needed (minimum 5 plus
> 1.15x safety) for 400 jobs waiting, 2.00/s arriving`

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — the design and why it is shaped this way
- [`docs/interactions.md`](docs/interactions.md) — sequence diagrams for registration, a cycle, a settings change, and a simulation run
- [`docs/development.md`](docs/development.md) — working on it, adding a platform, adding a setting
- [`api/openapi.yaml`](api/openapi.yaml) — the HTTP contract, checked against the routes by a test

## Running

```bash
make test     # unit tests; no cluster, no daemon, no network
make check    # what CI runs
make build    # build ./bin/autoscaler
make run      # run locally on :8080
```

Deployment is `deploy/chart`. Two values have no defaults on purpose:
`auth.token` (a service holding platform credentials must not ship with a
well-known one) and `persistence.storageClassName` (unset on a cluster with no
default class produces a PVC that stays Pending with nothing explaining why).
