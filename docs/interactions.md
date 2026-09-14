# How the pieces talk to each other

Four sequences cover everything this service does. They are drawn from the
code, not from intent — if one of them stops matching, the code is what
changed.

## 1. Registering a target

Nothing is stored until the platform has confirmed the access keys work. A
mistyped Deployment name or an expired token is refused while someone is
looking at the response, rather than discovered during the first burst the
target was supposed to absorb.

```mermaid
sequenceDiagram
    participant Op as Operator / simlab-api
    participant API as api
    participant Reg as registry
    participant Ad as platform adapter
    participant K8s as Kubernetes / ColonyOS / Docker

    Op->>API: POST /v1/targets {target, settings}
    API->>Reg: Create(target, settings)
    Reg->>Reg: validate id (usable as a resource name)
    Reg->>Reg: config.NewStore(settings) — refuse settings that cannot run
    Reg->>Ad: Validate(target)
    Ad->>K8s: reach the API / ping the daemon / check colony membership
    K8s-->>Ad: 200, or a refusal
    alt credentials work
        Ad-->>Reg: ok
        Reg->>Reg: store target + settings, persist (0600, atomic)
        Reg-->>API: snapshot (credentials redacted)
        API-->>Op: 201 Created
    else credentials do not
        Ad-->>Reg: error
        Reg-->>API: not usable
        API-->>Op: 400 with the platform's own reason
    end
```

## 2. One control cycle

The same sequence whether the cycle came from the runner's ticker or from a
`POST /cycle`. That is the point: a simulation exercises this path, not a
model of it.

```mermaid
sequenceDiagram
    participant Src as Runner (autonomous) / caller (driven)
    participant Ctl as controller
    participant Reg as registry
    participant Ad as platform adapter
    participant Pol as policy

    Src->>Ctl: Cycle(targetID, {at, workload?})
    Ctl->>Reg: Get target, Settings, Credentials
    Note over Ctl: one settings version for the whole cycle,<br/>so a mid-cycle change cannot split a decision
    Ctl->>Ad: Observe(target) — under this cycle's clock
    Ad-->>Ctl: capacity, and the queue if this platform sees one

    alt driven, and a workload was supplied
        Note over Ctl: the supplied queue is used
    else autonomous
        Note over Ctl: the platform's own queue is used,<br/>whatever a caller claims
    end

    Ctl->>Pol: Decide(state, loop memory, settings)
    Pol->>Pol: Required() — smallest count with no predicted breach,<br/>each candidate simulated on the ramp it actually arrives on
    Pol->>Pol: SplitTiers() — local to its cap, cloud as proven overflow
    Pol->>Pol: hysteresis — cooldowns, step limits, cloud lifetime
    Pol-->>Ctl: decision + next loop memory

    alt plan unchanged, or dry run
        Note over Ctl: nothing sent to the platform
    else
        Ctl->>Ad: Apply(target, plan)
        Ad-->>Ctl: what the platform now holds
    end

    Ctl->>Reg: RecordCycle(loop, decision, error?)
    Ctl-->>Src: decision, observation, applied
```

## 3. Changing settings while it runs

The headline property: no restart, no reload, and the change is in force on
the very next cycle.

```mermaid
sequenceDiagram
    participant Op as Operator
    participant API as api
    participant Reg as registry
    participant St as config.Store
    participant Ctl as controller (next cycle)

    Op->>API: PATCH /settings?expected_version=7 {"local_executor_cap": 80}
    API->>Reg: ApplySettings(patch, expected=7, actor)
    Reg->>St: Apply(patch, 7)
    St->>St: version still 7?
    alt someone else wrote first
        St-->>Reg: ErrVersionConflict
        Reg-->>API: conflict
        API-->>Op: 409 — you edited 7, current is 8
    else
        St->>St: patch onto current, validate the whole candidate
        alt the candidate cannot run
            St-->>Reg: rejected
            API-->>Op: 400 — the engine keeps the settings it had
        else
            St->>St: swap the pointer, append to the change log
            St-->>Reg: version 8
            Reg->>Reg: persist synchronously
            Reg-->>API: snapshot + warnings
            API-->>Op: 200
        end
    end

    Note over Ctl: the next cycle reads version 8 and<br/>stamps it on the decision it produces
```

## 4. A simulation run driving the real engine

This is how simlab exercises production logic without a cluster. Only the last
hop differs from a live target.

```mermaid
sequenceDiagram
    participant Web as simlab-web
    participant Sim as simlab-api
    participant API as autoscaler api
    participant Ctl as controller
    participant Adp as simulation adapter

    Web->>Sim: start a run
    loop each simulated cycle
        Sim->>Sim: advance the run's own clock, build the queue from the log
        Sim->>API: POST /cycle {at: simulated, workload}
        API->>Ctl: Cycle(...)
        Ctl->>Adp: Observe — fleet aged on the run's clock
        Adp-->>Ctl: capacity, coldstarts honoured in compressed time
        Ctl->>Ctl: the real engine, the real settings, the real hysteresis
        Ctl->>Adp: Apply(plan) — recorded, nothing created
        Ctl-->>API: decision
        API-->>Sim: decision + reason + projection
        Sim->>Sim: store the cycle in Postgres
        Sim-->>Web: stream it
    end
```

The substitution happens at exactly one point — the adapter — and everything
above it is the code that runs in production.
