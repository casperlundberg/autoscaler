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

## Why "simulate the queue forward"

A threshold rule ("scale up when the queue is over N") cannot answer the
question the SLA actually asks, which is *will anything miss its deadline?*
So the policy engine simulates: given the current per-priority queue, the
observed arrival rate, and the throughput one executor delivers, it plays the
queue forward over the decision horizon and checks whether any priority level
breaches its deadline. If nothing breaches, hold. If something breaches, it
searches for the smallest executor count that prevents it, then splits that
count across tiers — local first up to the cap, cloud only as proven overflow.

This has a useful consequence: the same engine that drives a real cluster can
be pointed at recorded data and asked what it *would* have done, and the answer
is produced by the identical code path, not a reimplementation.

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
