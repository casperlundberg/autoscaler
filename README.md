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

## The rest of the platform

This service is useful on its own — anything that can describe a queue can
drive it. It was built alongside two others, in sibling repositories:

- **simlab-api** — replays mining workloads against this service and records
  every decision, so two policies can be compared on identical work.
- **simlab-web** — the browser app for defining runs, reading their timelines,
  and editing a live target's settings.
- **platform-deploy** — what the platform needs in order to run: the umbrella
  chart, per-cluster values, the credential inventory, and `verify.sh`, which
  runs all of it for real and checks the result.

## Versions

Releases are [semantic versions](https://semver.org), tagged `vMAJOR.MINOR.PATCH`
and cut with

```bash
make release VERSION=1.3.0
```

which refuses a dirty tree, a branch other than `main`, a `main` behind its
remote, a version not above the last release, a `CHANGELOG.md` with no section
for it, and failing checks — then tags, with the changelog section as the tag
message, and pushes the commit and the tag together so CI stamps the image with
the release. Between releases a build is `1.3.1-dev.N+<commit>`, and `.dirty`
when built with uncommitted changes (`make version` prints it). Every binary
carries its version and commit; `GET /v1/version` reports them.

For a controller whose decisions are research results, a version answers one
question besides "will my client break": **will the same observation get the
same decision?**

- **MAJOR** — the HTTP API changes incompatibly, or a decision for the same
  observation, settings and history can change.
- **MINOR** — something is added (an endpoint, a setting, a platform) and every
  existing decision is as it was.
- **PATCH** — a fix that changes no decision and no contract.

Every recorded Simlab run names the autoscaler build that decided it, so a
decision can always be traced to its commit and rebuilt from it.

## Deployment

`deploy/chart`. Two values have no defaults on purpose:
`auth.token` (a service holding platform credentials must not ship with a
well-known one) and `persistence.storageClassName` (unset on a cluster with no
default class produces a PVC that stays Pending with nothing explaining why).
