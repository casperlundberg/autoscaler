// Package platform is the boundary between deciding and doing.
//
// The decision engine works out how many executors a workload needs. What an
// executor *is* — a Kubernetes pod, a ColonyOS executor running as a pod, a
// ColonyOS executor running as a standalone container, or a row in a
// simulation — lives entirely behind the Provisioner interface defined here.
// Adding a platform means adding an adapter; it never means touching the
// engine.
package platform

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

// Kind names a platform.
type Kind string

const (
	// KindSimulation computes decisions and reports back what they would have
	// provisioned, without touching anything.
	KindSimulation Kind = "simulation"

	// KindKubernetes scales plain Kubernetes Deployments.
	KindKubernetes Kind = "kubernetes"

	// KindColonyOSPods runs ColonyOS executors as Kubernetes pods.
	KindColonyOSPods Kind = "colonyos-k8s"

	// KindColonyOSContainers runs ColonyOS executors as standalone containers
	// on a Docker host, with no orchestrator involved.
	KindColonyOSContainers Kind = "colonyos-container"
)

// Mode says who drives a target's control loop.
type Mode string

const (
	// ModeAutonomous means this service runs the loop itself: it polls the
	// platform for the queue, decides, and provisions, on its own schedule.
	ModeAutonomous Mode = "autonomous"

	// ModeDriven means someone else supplies the observation and asks for a
	// decision. A simulation run works this way, and so does any platform that
	// cannot see its own queue.
	ModeDriven Mode = "driven"
)

// Target is one scalable workload: which platform, how to reach it, the access
// keys to act on it, and who drives its loop.
type Target struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`

	// Mode defaults to driven, which is the safe default: a target only starts
	// provisioning on its own once somebody says it should.
	Mode Mode `json:"mode"`

	// Config is the platform's non-secret settings — namespace, deployment
	// names, image, colony name. Its keys are defined by the adapter's Schema.
	Config map[string]string `json:"config"`

	// Credentials never leave this struct in readable form; see package
	// secret.
	Credentials secret.Bundle `json:"credentials"`
}

// Workload is the queue an adapter can see for itself.
type Workload struct {
	Queues map[domain.Priority]domain.QueueInfo `json:"queues"`

	// BurstExempt is waiting work that may use capacity but may not be the
	// reason cloud capacity is bought; see domain.SystemState.
	BurstExempt map[domain.Priority]domain.QueueInfo `json:"burst_exempt,omitempty"`

	ExecutorThroughput float64 `json:"executor_throughput_per_second"`
}

// Observation is what an adapter reports back.
type Observation struct {
	Capacity domain.Capacity `json:"capacity"`

	// Workload is nil for a platform that cannot see the queue. Kubernetes is
	// the clear case: it knows how many pods are running, and nothing at all
	// about the jobs waiting for them. Such a target is driven — the queue
	// arrives with the request — rather than polled.
	Workload *Workload `json:"workload,omitempty"`
}

// ApplyResult is what actually happened when a plan was enacted.
type ApplyResult struct {
	// Applied is the capacity the platform now holds. It can differ from the
	// requested plan: a quota, an admission webhook, or a cap on the platform
	// side may allow less than was asked for, and the control loop needs the
	// truth rather than its own intent echoed back.
	Applied domain.Plan `json:"applied"`

	Changed bool   `json:"changed"`
	Detail  string `json:"detail,omitempty"`
}

// Provisioner is what every platform adapter implements. Four methods, chosen
// so that a new platform can be supported without the decision engine, the
// control loop or the API learning anything about it.
type Provisioner interface {
	// Kind is the platform this adapter serves.
	Kind() Kind

	// Schema declares the config and credential fields this adapter needs,
	// which is what lets the UI render a registration form it was never
	// specifically written for.
	Schema() Schema

	// Validate checks that a target's settings and access keys actually work,
	// at registration time rather than at 3am during a burst.
	Validate(ctx context.Context, target Target) error

	// Observe reports current capacity, and the queue if this platform can see
	// one.
	Observe(ctx context.Context, target Target) (Observation, error)

	// Apply makes the platform's capacity match the plan. Plans are absolute
	// counts, so applying the same plan twice is the same as applying it once.
	Apply(ctx context.Context, target Target, plan domain.Plan) (ApplyResult, error)
}

// Field is one setting an adapter needs, described well enough for a form to
// be generated from it.
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
	Example     string `json:"example,omitempty"`
	Required    bool   `json:"required"`
}

// Schema describes what a platform needs in order to be driven.
//
// It exists so that "configurable for a new cluster platform" is a data
// question rather than a code question on the client side: the API serves
// these, and the settings UI renders a registration form for a platform it has
// never heard of.
type Schema struct {
	Kind    Kind   `json:"kind"`
	Summary string `json:"summary"`

	// SeesWorkload says whether Observe returns a queue. When false, the
	// target must be driven: whoever calls the autoscaler supplies the
	// workload with the request.
	SeesWorkload bool `json:"sees_workload"`

	// Config is the non-secret settings; Credentials is the access keys.
	// Which list a field appears in is what marks it secret.
	Config      []Field `json:"config"`
	Credentials []Field `json:"credentials"`
}

// Check validates a target against the schema, reporting every problem at
// once. An operator registering a target should not have to discover three
// missing fields through three failed requests.
func (s Schema) Check(t Target) error {
	var problems []string

	switch t.Mode {
	case "", ModeDriven, ModeAutonomous:
	default:
		problems = append(problems, fmt.Sprintf("mode %q is not one of %q or %q",
			t.Mode, ModeAutonomous, ModeDriven))
	}
	if t.Mode == ModeAutonomous && !s.SeesWorkload {
		// An autonomous loop has to be able to see the work it is scaling for.
		// The alternative — polling a platform that reports only capacity —
		// would decide from an empty queue every cycle and scale everything to
		// its floor.
		problems = append(problems, fmt.Sprintf(
			"mode %q needs a platform that can see its own queue, and %s cannot: "+
				"use %q and supply the workload with each decision",
			ModeAutonomous, s.Kind, ModeDriven))
	}

	known := make(map[string]bool, len(s.Config))
	for _, f := range s.Config {
		known[f.Name] = true
		if f.Required && strings.TrimSpace(t.Config[f.Name]) == "" && f.Default == "" {
			problems = append(problems, fmt.Sprintf("config.%s is required (%s)", f.Name, f.Label))
		}
	}

	// An unrecognised key is a typo often enough to be worth refusing. Ignored
	// silently, it produces a target that looks configured, is accepted, and
	// then behaves as though the setting was never given.
	var unknown []string
	for name := range t.Config {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		problems = append(problems, fmt.Sprintf("config.%s is not a setting of the %s platform",
			name, s.Kind))
	}

	for _, f := range s.Credentials {
		if !f.Required {
			continue
		}
		if v, ok := t.Credentials.Get(f.Name); !ok || strings.TrimSpace(v) == "" {
			problems = append(problems, fmt.Sprintf("credentials.%s is required (%s)", f.Name, f.Label))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("target is not usable on the %s platform: %s",
		s.Kind, strings.Join(problems, "; "))
}

// ConfigValue is a target's setting, or the schema's default when the target
// does not set it. Adapters read settings through here so that a default is
// declared once, in the schema the UI also renders, rather than repeated at
// every use.
func (s Schema) ConfigValue(t Target, name string) string {
	if v, ok := t.Config[name]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	for _, f := range s.Config {
		if f.Name == name {
			return f.Default
		}
	}
	return ""
}

// ConfigInt reads a numeric setting.
func (s Schema) ConfigInt(t Target, name string) (int, error) {
	raw := s.ConfigValue(t, name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config.%s must be a whole number, got %q", name, raw)
	}
	return n, nil
}

// ConfigBool reads a boolean setting, accepting the spellings people actually
// type into a form.
func (s Schema) ConfigBool(t Target, name string) bool {
	switch strings.ToLower(strings.TrimSpace(s.ConfigValue(t, name))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// Registry maps a platform kind to the adapter that serves it.
type Registry struct {
	byKind map[Kind]Provisioner
}

// NewRegistry refuses duplicate kinds: two adapters claiming one platform
// would make which of them runs depend on map ordering.
func NewRegistry(provisioners ...Provisioner) (*Registry, error) {
	byKind := make(map[Kind]Provisioner, len(provisioners))
	for _, p := range provisioners {
		if _, exists := byKind[p.Kind()]; exists {
			return nil, fmt.Errorf("two adapters registered for platform %q", p.Kind())
		}
		byKind[p.Kind()] = p
	}
	return &Registry{byKind: byKind}, nil
}

// Get returns the adapter for a kind. The error lists what is available,
// because the caller has almost always just mistyped a platform name.
func (r *Registry) Get(kind Kind) (Provisioner, error) {
	if p, ok := r.byKind[kind]; ok {
		return p, nil
	}
	available := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		available = append(available, string(k))
	}
	sort.Strings(available)
	return nil, fmt.Errorf("no adapter for platform %q; registered platforms are: %s",
		kind, strings.Join(available, ", "))
}

// Kinds is every registered platform, sorted.
func (r *Registry) Kinds() []Kind {
	out := make([]Kind, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Schemas is every registered platform's schema, in the same stable order.
func (r *Registry) Schemas() []Schema {
	out := make([]Schema, 0, len(r.byKind))
	for _, k := range r.Kinds() {
		out = append(out, r.byKind[k].Schema())
	}
	return out
}
