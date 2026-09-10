package colonycontainers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
)

// Field names, in one place so the schema, the adapter and the errors agree.
const (
	FieldDockerHost      = "docker_host"
	FieldExecutorImage   = "executor_image"
	FieldContainerPrefix = "container_prefix"
	FieldNetwork         = "network"
	FieldRestartPolicy   = "restart_policy"
	FieldCPUs            = "cpus"
	FieldMemory          = "memory"
	FieldStopGrace       = "stop_grace_seconds"
	FieldTimeout         = "request_timeout_seconds"

	FieldColoniesHost       = "colonies_host"
	FieldColoniesPort       = "colonies_port"
	FieldColoniesTLS        = "colonies_tls"
	FieldColoniesSkipVerify = "colonies_skip_tls_verify"
	FieldColonyName         = "colony_name"
	FieldExecutorType       = "executor_type"
	FieldMaxProcesses       = "max_processes"
	FieldArrivalWindow      = "arrival_window_seconds"
	FieldJobSecondsKey      = "job_seconds_env_key"
	FieldDefaultJobSeconds  = "default_job_seconds"

	CredentialColoniesKey  = "colonies_prvkey"
	CredentialDockerCert   = "docker_tls_cert"
	CredentialDockerKey    = "docker_tls_key"
	CredentialDockerCA     = "docker_tls_ca"
	CredentialRegistryAuth = "registry_auth"
)

const (
	tierLocal = "local"
	tierCloud = "cloud"

	labelTarget = "autoscaler.target"
	labelTier   = "autoscaler.tier"
)

// Adapter provisions ColonyOS executors as plain Docker containers.
type Adapter struct{}

// New returns the adapter.
func New() *Adapter { return &Adapter{} }

// Kind identifies this adapter.
func (a *Adapter) Kind() platform.Kind { return platform.KindColonyOSContainers }

// Schema declares what a Docker-hosted executor pool needs.
func (a *Adapter) Schema() platform.Schema {
	return platform.Schema{
		Kind: platform.KindColonyOSContainers,
		Summary: "Runs ColonyOS executors as standalone containers on a Docker host, " +
			"with no orchestrator. Note that Docker outside Swarm has no secret " +
			"store, so the executor key is passed as an environment variable and " +
			"is visible to anyone who can inspect containers on that host.",
		SeesWorkload: true,
		Config: []platform.Field{
			{Name: FieldDockerHost, Label: "Docker host", Required: true,
				Description: "unix:// for a local daemon, tcp:// for a remote one.",
				Example:     "unix:///var/run/docker.sock"},
			{Name: FieldExecutorImage, Label: "Executor image", Required: true,
				Example: "ghcr.io/example/colonyos-executor:v1"},
			{Name: FieldContainerPrefix, Label: "Container name prefix",
				Description: "Defaults to executor-<target id>."},
			{Name: FieldNetwork, Label: "Docker network",
				Description: "The network the executors join, so they can reach the ColonyOS server."},
			{Name: FieldRestartPolicy, Label: "Restart policy",
				Description: "Docker restart policy for executor containers.", Default: "on-failure"},
			{Name: FieldCPUs, Label: "CPUs per executor", Example: "0.5"},
			{Name: FieldMemory, Label: "Memory per executor", Example: "512m"},
			{Name: FieldStopGrace, Label: "Stop grace period (seconds)",
				Description: "How long an executor is given to finish its current job before being killed.",
				Default:     "30"},
			{Name: FieldTimeout, Label: "Request timeout (seconds)", Default: "30"},

			{Name: FieldColoniesHost, Label: "ColonyOS server host", Required: true},
			{Name: FieldColoniesPort, Label: "ColonyOS server port", Default: "50080"},
			{Name: FieldColoniesTLS, Label: "ColonyOS server uses TLS", Default: "false"},
			{Name: FieldColoniesSkipVerify, Label: "Skip ColonyOS TLS verification", Default: "false"},
			{Name: FieldColonyName, Label: "Colony name", Required: true, Example: "dev"},
			{Name: FieldExecutorType, Label: "Executor type", Required: true,
				Example: "bemis-storhall"},
			{Name: FieldMaxProcesses, Label: "Maximum processes read per cycle", Default: "10000"},
			{Name: FieldArrivalWindow, Label: "Arrival rate window (seconds)", Default: "300"},
			{Name: FieldJobSecondsKey, Label: "Execution time env key", Default: "exec_seconds"},
			{Name: FieldDefaultJobSeconds, Label: "Default execution time (seconds)", Default: "30"},
		},
		Credentials: []platform.Field{
			{Name: CredentialColoniesKey, Label: "ColonyOS executor private key", Required: true,
				Description: "Hex-encoded. Passed to the containers as an environment variable."},
			{Name: CredentialDockerCert, Label: "Docker client certificate (PEM)",
				Description: "Required for a TLS-protected tcp:// daemon."},
			{Name: CredentialDockerKey, Label: "Docker client key (PEM)"},
			{Name: CredentialDockerCA, Label: "Docker CA certificate (PEM)"},
			{Name: CredentialRegistryAuth, Label: "Registry auth token",
				Description: "Base64 X-Registry-Auth value, for pulling from a private registry."},
		},
	}
}

// Validate proves the daemon and the colony are both reachable.
func (a *Adapter) Validate(ctx context.Context, target platform.Target) error {
	var problems []string
	if err := a.Schema().Check(target); err != nil {
		problems = append(problems, err.Error())
	}

	docker, err := a.dockerClient(target)
	if err != nil {
		problems = append(problems, err.Error())
	}
	colonies, identity, err := a.coloniesClient(target)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	if err := docker.ping(ctx); err != nil {
		return fmt.Errorf("the docker host is not usable: %w", err)
	}
	if err := colonies.CheckAccess(ctx, a.Schema().ConfigValue(target, FieldColonyName), identity); err != nil {
		return fmt.Errorf("colonies is not usable with this key: %w", err)
	}
	return nil
}

// Observe counts this target's containers and reads its colony queue.
func (a *Adapter) Observe(ctx context.Context, target platform.Target) (platform.Observation, error) {
	docker, err := a.dockerClient(target)
	if err != nil {
		return platform.Observation{}, err
	}

	localReady, localPending, err := a.tierCapacity(ctx, docker, target, tierLocal)
	if err != nil {
		return platform.Observation{}, err
	}
	cloudReady, cloudPending, err := a.tierCapacity(ctx, docker, target, tierCloud)
	if err != nil {
		return platform.Observation{}, err
	}

	workload, err := a.observeWorkload(ctx, target)
	if err != nil {
		return platform.Observation{}, err
	}

	return platform.Observation{
		Capacity: domain.Capacity{
			LocalReady: localReady, LocalPending: localPending,
			CloudReady: cloudReady, CloudPending: cloudPending,
		},
		Workload: workload,
	}, nil
}

// Apply makes each tier's container count match the plan.
func (a *Adapter) Apply(ctx context.Context, target platform.Target,
	plan domain.Plan) (platform.ApplyResult, error) {
	if plan.LocalExecutors < 0 || plan.CloudExecutors < 0 {
		return platform.ApplyResult{}, fmt.Errorf("plan has a negative executor count: %+v", plan)
	}

	docker, err := a.dockerClient(target)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	localChanged, err := a.applyTier(ctx, docker, target, tierLocal, plan.LocalExecutors)
	if err != nil {
		return platform.ApplyResult{}, err
	}
	cloudChanged, err := a.applyTier(ctx, docker, target, tierCloud, plan.CloudExecutors)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	return platform.ApplyResult{
		Applied: plan,
		Changed: localChanged || cloudChanged,
		Detail: fmt.Sprintf("%s: %d local, %d cloud containers",
			a.prefix(target), plan.LocalExecutors, plan.CloudExecutors),
	}, nil
}

func (a *Adapter) applyTier(ctx context.Context, docker *dockerClient, target platform.Target,
	tier string, want int) (bool, error) {
	existing, err := docker.listContainers(ctx, a.tierLabels(target, tier))
	if err != nil {
		return false, fmt.Errorf("listing %s containers: %w", tier, err)
	}

	// Oldest first, so the surplus is taken from the end. The oldest
	// container has finished starting and may be mid-job; the newest has done
	// the least work and costs the least to discard.
	sort.Slice(existing, func(i, j int) bool {
		if existing[i].Created != existing[j].Created {
			return existing[i].Created < existing[j].Created
		}
		return existing[i].Name < existing[j].Name
	})

	switch {
	case want > len(existing):
		return true, a.startMore(ctx, docker, target, tier, existing, want)
	case want < len(existing):
		return true, a.removeSurplus(ctx, docker, target, existing[want:])
	default:
		return false, nil
	}
}

func (a *Adapter) startMore(ctx context.Context, docker *dockerClient, target platform.Target,
	tier string, existing []container, want int) error {
	schema := a.Schema()
	image := schema.ConfigValue(target, FieldExecutorImage)
	registryAuth, _ := target.Credentials.Get(CredentialRegistryAuth)

	// A Docker host has no image controller working on its behalf, so on a
	// machine that has never run this executor every create would fail.
	if err := docker.pullImage(ctx, image, registryAuth); err != nil {
		return fmt.Errorf("pulling %s: %w", image, err)
	}

	taken := make(map[string]bool, len(existing))
	for _, c := range existing {
		taken[c.Name] = true
	}

	for created := len(existing); created < want; created++ {
		name := a.nextName(target, tier, taken)
		taken[name] = true

		spec, err := a.containerSpec(target, tier, name)
		if err != nil {
			return err
		}

		id, err := docker.createContainer(ctx, spec)
		if err != nil {
			return fmt.Errorf("creating container %s: %w", name, err)
		}
		// Created is not running. A container that exists but was never
		// started does no work, and counting it as capacity would stall the
		// queue with no visible failure anywhere.
		if err := docker.startContainer(ctx, id); err != nil {
			return fmt.Errorf("starting container %s: %w", name, err)
		}
	}
	return nil
}

func (a *Adapter) removeSurplus(ctx context.Context, docker *dockerClient, target platform.Target,
	surplus []container) error {
	grace, err := a.Schema().ConfigInt(target, FieldStopGrace)
	if err != nil {
		return err
	}

	for _, c := range surplus {
		if c.running() {
			if err := docker.stopContainer(ctx, c.ID, time.Duration(grace)*time.Second); err != nil {
				return fmt.Errorf("stopping container %s: %w", c.Name, err)
			}
		}
		// Removed, not merely stopped: a stopped container keeps its name, and
		// the next scale-up would collide with it and fail.
		if err := docker.removeContainer(ctx, c.ID); err != nil {
			return fmt.Errorf("removing container %s: %w", c.Name, err)
		}
	}
	return nil
}

func (a *Adapter) containerSpec(target platform.Target, tier, name string) (containerSpec, error) {
	schema := a.Schema()

	key, ok := target.Credentials.Get(CredentialColoniesKey)
	if !ok || strings.TrimSpace(key) == "" {
		return containerSpec{}, fmt.Errorf("credentials.%s is required to start executors",
			CredentialColoniesKey)
	}

	env := []string{
		"COLONIES_SERVER_HOST=" + schema.ConfigValue(target, FieldColoniesHost),
		"COLONIES_SERVER_PORT=" + schema.ConfigValue(target, FieldColoniesPort),
		"COLONIES_SERVER_TLS=" + schema.ConfigValue(target, FieldColoniesTLS),
		"COLONIES_COLONY_NAME=" + schema.ConfigValue(target, FieldColonyName),
		"COLONIES_EXECUTOR_TYPE=" + schema.ConfigValue(target, FieldExecutorType),
		// Each executor registers with the colony under this name, so two
		// containers sharing one would collide on registration.
		"COLONIES_EXECUTOR_NAME=" + name,
		"COLONIES_PRVKEY=" + key,
		"AUTOSCALER_TIER=" + tier,
	}

	return containerSpec{
		Name:          name,
		Image:         schema.ConfigValue(target, FieldExecutorImage),
		Env:           env,
		Labels:        a.tierLabels(target, tier),
		Network:       schema.ConfigValue(target, FieldNetwork),
		RestartPolicy: schema.ConfigValue(target, FieldRestartPolicy),
		CPUs:          schema.ConfigValue(target, FieldCPUs),
		Memory:        schema.ConfigValue(target, FieldMemory),
	}, nil
}

func (a *Adapter) tierCapacity(ctx context.Context, docker *dockerClient, target platform.Target,
	tier string) (ready, pending int, err error) {
	containers, err := docker.listContainers(ctx, a.tierLabels(target, tier))
	if err != nil {
		return 0, 0, fmt.Errorf("listing %s containers: %w", tier, err)
	}

	for _, c := range containers {
		if c.running() {
			ready++
			continue
		}
		// Created, restarting, paused: capacity on its way, exactly like a pod
		// that has not become Ready.
		pending++
	}
	return ready, pending, nil
}

func (a *Adapter) observeWorkload(ctx context.Context, target platform.Target) (*platform.Workload, error) {
	schema := a.Schema()

	colonies, identity, err := a.coloniesClient(target)
	if err != nil {
		return nil, err
	}

	colonyName := schema.ConfigValue(target, FieldColonyName)
	executorType := schema.ConfigValue(target, FieldExecutorType)
	maxProcesses, err := schema.ConfigInt(target, FieldMaxProcesses)
	if err != nil {
		return nil, err
	}

	waiting, err := colonies.GetProcesses(ctx, colonyName, colonyos.StateWaiting,
		executorType, maxProcesses, identity)
	if err != nil {
		return nil, fmt.Errorf("reading the waiting queue: %w", err)
	}
	running, err := colonies.GetProcesses(ctx, colonyName, colonyos.StateRunning,
		executorType, maxProcesses, identity)
	if err != nil {
		return nil, fmt.Errorf("reading the running processes: %w", err)
	}

	arrivalWindow, err := schema.ConfigInt(target, FieldArrivalWindow)
	if err != nil {
		return nil, err
	}
	defaultJobSeconds, err := schema.ConfigInt(target, FieldDefaultJobSeconds)
	if err != nil {
		return nil, err
	}

	workload, err := colonyos.BuildWorkload(waiting, running, colonyos.WorkloadOptions{
		Now:               platform.CycleTime(ctx),
		ArrivalWindow:     time.Duration(arrivalWindow) * time.Second,
		JobSecondsEnvKey:  schema.ConfigValue(target, FieldJobSecondsKey),
		DefaultJobSeconds: float64(defaultJobSeconds),
	})
	if err != nil {
		return nil, err
	}
	return &workload, nil
}

func (a *Adapter) dockerClient(target platform.Target) (*dockerClient, error) {
	schema := a.Schema()

	timeoutSeconds, err := schema.ConfigInt(target, FieldTimeout)
	if err != nil {
		return nil, err
	}

	certificate, _ := target.Credentials.Get(CredentialDockerCert)
	key, _ := target.Credentials.Get(CredentialDockerKey)
	ca, _ := target.Credentials.Get(CredentialDockerCA)

	return newDockerClient(schema.ConfigValue(target, FieldDockerHost),
		dockerTLS{Certificate: certificate, Key: key, CA: ca},
		time.Duration(timeoutSeconds)*time.Second)
}

func (a *Adapter) coloniesClient(target platform.Target) (*colonyos.Client, *colonyos.Identity, error) {
	schema := a.Schema()

	port, err := schema.ConfigInt(target, FieldColoniesPort)
	if err != nil {
		return nil, nil, err
	}
	timeoutSeconds, err := schema.ConfigInt(target, FieldTimeout)
	if err != nil {
		return nil, nil, err
	}

	client, err := colonyos.NewClient(colonyos.ServerConfig{
		Host:          schema.ConfigValue(target, FieldColoniesHost),
		Port:          port,
		TLS:           schema.ConfigBool(target, FieldColoniesTLS),
		SkipTLSVerify: schema.ConfigBool(target, FieldColoniesSkipVerify),
		Timeout:       time.Duration(timeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}

	key, _ := target.Credentials.Get(CredentialColoniesKey)
	identity, err := colonyos.NewIdentity(key)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials.%s is not a usable ColonyOS key: %w",
			CredentialColoniesKey, err)
	}
	return client, identity, nil
}

// tierLabels are what make this target's containers findable, and — just as
// importantly — keep another target's containers on the same host from being
// counted as capacity this autoscaler controls.
func (a *Adapter) tierLabels(target platform.Target, tier string) map[string]string {
	return map[string]string{labelTarget: target.ID, labelTier: tier}
}

func (a *Adapter) prefix(target platform.Target) string {
	if prefix := a.Schema().ConfigValue(target, FieldContainerPrefix); prefix != "" {
		return prefix
	}
	return "executor-" + target.ID
}

// nextName finds the lowest unused index, so names stay short and predictable
// across scale-ups and scale-downs instead of drifting ever upward.
func (a *Adapter) nextName(target platform.Target, tier string, taken map[string]bool) string {
	base := fmt.Sprintf("%s-%s-", a.prefix(target), tier)
	for index := 1; ; index++ {
		name := base + strconv.Itoa(index)
		if !taken[name] {
			return name
		}
	}
}

// parseMemory reads Docker's own size suffixes into bytes.
func parseMemory(value string) (int64, error) {
	multipliers := []struct {
		suffix string
		factor int64
	}{
		{"gb", 1 << 30}, {"g", 1 << 30},
		{"mb", 1 << 20}, {"m", 1 << 20},
		{"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	}

	trimmed := strings.ToLower(strings.TrimSpace(value))
	for _, m := range multipliers {
		if number, found := strings.CutSuffix(trimmed, m.suffix); found {
			amount, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, fmt.Errorf("config.%s=%q is not a size", FieldMemory, value)
			}
			return int64(amount * float64(m.factor)), nil
		}
	}

	amount, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config.%s=%q is not a size", FieldMemory, value)
	}
	return amount, nil
}

// parseCPUs converts a CPU count into the nano-CPU units Docker takes.
func parseCPUs(value string) (int64, error) {
	amount, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("config.%s=%q is not a number of CPUs", FieldCPUs, value)
	}
	return int64(amount * 1e9), nil
}
