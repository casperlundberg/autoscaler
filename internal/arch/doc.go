// Package arch holds no code. It exists so that the layering rules in
// CLAUDE.md and docs/architecture.md are enforced by the build rather than by
// everyone remembering them.
//
// The rules were true when they were written and still mostly true when they
// were checked, but "registry imports both policy and platform" had already
// crept past a documented invariant that says the controller is the only place
// the two meet. A rule nothing checks is a rule that decays quietly, and this
// one is load-bearing: it is what keeps policy a set of pure functions that a
// simulated run and a production run can share.
package arch
