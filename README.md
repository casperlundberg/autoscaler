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

## Status

Under construction. See `docs/architecture.md` for the design and
`api/openapi.yaml` for the HTTP contract.

## Running

```bash
make test     # unit tests
make build    # build ./bin/autoscaler
make run      # run locally on :8080
```
