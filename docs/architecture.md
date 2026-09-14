# Architecture

## The problem this service solves

A batch workload arrives with per-job priorities, and each priority carries an
SLA deadline. Capacity comes from two tiers: a fixed amount of on-premise
hardware, and an elastic cloud tier that is slower to start and costs money.
Something has to decide, continuously, how many executors to run in each tier.

That decision is the same regardless of whether the executors are Kubernetes
pods, ColonyOS executors, or rows in a simulator. What differs is *how you
count what is running* and *how you ask for more*. That split is the spine of
this service.

## Layers

```
        ┌──────────────────────────────────────────────┐
        │ api/            HTTP + OpenAPI               │
        └───────────────┬──────────────────────────────┘
                        │
        ┌───────────────▼──────────────────────────────┐
        │ controller/     one control loop per target  │
        │                 observe → decide → enact     │
        └──────┬─────────────────────┬─────────────────┘
               │                     │
   ┌───────────▼──────────┐ ┌────────▼─────────────────┐
   │ policy/              │ │ platform/                │
   │ simulate the queue   │ │ Provisioner interface    │
   │ forward; find the    │ │  ├── kubernetes          │
   │ smallest executor    │ │  ├── colonyos-k8s        │
   │ count with no breach │ │  ├── colonyos-container  │
   └───────────┬──────────┘ │  └── simulation          │
               │            └────────┬─────────────────┘
   ┌───────────▼──────────┐ ┌────────▼─────────────────┐
   │ domain/              │ │ target/ + secret/        │
   │ SystemState          │ │ platform binding,        │
   │ ScalingDecision      │ │ credentials, redaction   │
   │ Priority, SLA        │ └──────────────────────────┘
   └──────────────────────┘
               ▲
   ┌───────────┴──────────┐
   │ config/              │
   │ versioned settings,  │
   │ validated, hot-swap  │
   └──────────────────────┘
```

Dependencies point downward only. `domain/` imports nothing of ours;
`policy/` imports `domain/` and `config/`; `platform/` imports `domain/` and
never imports `policy/`. The controller is the only place the two meet.

Those rules are checked by the build, not by memory: `internal/arch` reads the
import graph and fails on a dependency that crosses a layer. It was added
after `registry` was found importing both `policy` and `platform` — only for a
data type, but past an invariant stated as absolute. `LoopState` moved to
`domain/` as part of that, which is also where it belonged: `Decide` returns a
`domain.Decision`, and the memory it hands back beside it is the same kind of
vocabulary.

## Why "simulate the queue forward"

A threshold rule ("scale up when the queue is over N") cannot answer the
question the SLA actually asks, which is *will anything miss its deadline?*
So the policy engine simulates: given the current per-priority queue, the
observed arrival rate, and the throughput one executor delivers, it plays the
queue forward over the decision horizon and checks whether any priority level
breaches its deadline. If nothing breaches, hold. If something breaches, it
searches for the smallest executor count that prevents it, then splits that
count across tiers — local first up to the cap, cloud only as proven overflow.

### Capacity arrives late, and the simulation says so

An executor that has been requested is not an executor that is working. It has
to be scheduled, pulled, started and registered first, and on the cloud tier
that is minutes. So a candidate count is not simulated as a flat number:
`AvailabilityOf` turns a plan into a ramp — what can serve now, and what joins
once its tier's coldstart has elapsed — and `Simulate` serves only what is
available at each step.

Without that, the engine answers a question nobody asks: *what count would
avoid this breach if capacity were instant*. It is most confidently wrong in
the one situation that matters, a cold pool during a ramp, and it is wrong in
the optimistic direction.

An observation reports how many executors are pending, never how long they
have been, so a pending executor is charged its whole coldstart again. That is
pessimistic by up to one coldstart, and deliberately: the error can only hold
capacity that was not strictly needed, never miss a deadline that could have
been met. Reporting pending ages from the adapters would remove it.

### Three outcomes, not two

Modelling the ramp makes "no executor count avoids this breach" an ordinary
state rather than an emergency, so the engine has to say *which* kind it is:

| Outcome | What it means | What it asks for |
|---|---|---|
| `Achievable` | Some count inside the caps avoids every breach | That count, plus headroom |
| `Unavoidable` | Nothing can start inside the deadline; the breach is committed | What the queue needs once capacity arrives |
| `Overloaded` | Arrivals outrun both caps serving from the first instant | The ceiling |

Telling the middle case from the last one is what stops a cold pool waking up
and provisioning both tiers to their caps against a breach that no amount of
capacity could have dodged. The reasoning string names which case fired, so a
run log shows a modest executor count beside an unavoidable breach and says
why that was the right answer rather than looking like under-provisioning.

This all has a useful consequence: the same engine that drives a real cluster
can be pointed at recorded data and asked what it *would* have done, and the
answer is produced by the identical code path, not a reimplementation.

## The Provisioner interface

Every platform adapter implements the same small interface:

- `Observe(ctx, target)` — how many executors are running, ready, and pending
- `Apply(ctx, target, plan)` — make the running count match the plan
- `Validate(credentials)` — check the access keys before anything is stored

`simulation` is a first-class adapter, not a test double. It accepts a plan,
records it, and reports back the capacity that plan would have produced. This
is what lets a simulation run exercise the real decision engine without
touching a cluster.

## Settings at runtime

Settings live in a versioned store behind a read-write lock. A write validates
the whole candidate document, then swaps a pointer; readers always see one
coherent version, never a half-applied edit. Writes carry an expected version
so two concurrent editors cannot silently overwrite each other, and every
accepted change is appended to an audit log with the fields that moved.

## Credentials

Access keys arrive over the API when a target is registered, are validated
against the platform immediately, and are never returned. Reads show the key
names present, a SHA-256 fingerprint prefix, and nothing else — enough to tell
whether a key changed, not enough to use it.
