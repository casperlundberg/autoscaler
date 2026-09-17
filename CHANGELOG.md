# Changelog

Every release, newest first. `make release` will not tag a version without a
section here, so the tag message and this file always agree.

## 1.0.0 — 2026-09-17

The first versioned release. 1.0.0 rather than 0.x because the controller
already makes the decisions recorded as research results, and a version has to
be able to promise something about them from here on.

- Scaling decisions by simulating the queue forward per priority level and
  searching for the smallest capacity with no predicted breach; on-premise
  capacity first, and only proven overflow to cloud, each decision with its
  reasoning.
- Coldstart-aware planning: when no capacity can start in time, it asks for what
  the queue needs once capacity arrives, and says so.
- Platform adapters for Kubernetes, ColonyOS as pods, ColonyOS as standalone
  containers, and simulation.
- Runtime settings as a versioned store with validate-then-swap and
  compare-and-swap writes; credentials never returned.
- `GET /v1/version`: the build's semantic version, commit, whether it was
  modified, Go version and platform, so a result can name the autoscaler that
  decided it.
