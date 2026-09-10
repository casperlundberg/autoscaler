package colonycontainers_test

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/colonycontainers"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos/colonytest"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

const (
	executorKey = "fcc79953d8a751bf41db661592dc34d30004b1a651ffa0725b03ac227641499d"
	executorID  = "039231c7644e04b6895471dd5335cf332681c54e27f81fac54f9067b3f2c0103"
)

var now = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

type stack struct {
	docker   *fakeDocker
	colonies *colonytest.Server
	target   platform.Target
}

func newStack(t *testing.T, overrides map[string]string) *stack {
	t.Helper()

	docker := newFakeDocker(t)
	dockerHost := docker.startOnSocket()

	colonies := colonytest.New(t, executorID)
	coloniesServer := colonies.Start()
	parsed, err := url.Parse(coloniesServer.URL)
	if err != nil {
		t.Fatalf("parsing colonies URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parsing colonies port: %v", err)
	}

	config := map[string]string{
		"docker_host":    dockerHost,
		"executor_image": "ghcr.io/example/colonyos-executor:v1",
		"colonies_host":  parsed.Hostname(),
		"colonies_port":  strconv.Itoa(port),
		"colony_name":    "dev",
		"executor_type":  "bemis-storhall",
	}
	for k, v := range overrides {
		if v == "" {
			delete(config, k)
			continue
		}
		config[k] = v
	}

	return &stack{
		docker:   docker,
		colonies: colonies,
		target: platform.Target{
			ID: "storhall", Name: "Storhall", Kind: platform.KindColonyOSContainers,
			Config:      config,
			Credentials: secret.NewBundle(map[string]string{"colonies_prvkey": executorKey}),
		},
	}
}

func at(when time.Time) context.Context {
	return platform.WithCycleTime(context.Background(), when)
}

func TestTheAdapterReachesADockerHostWithNoOrchestrator(t *testing.T) {
	schema := colonycontainers.New().Schema()

	if schema.Kind != platform.KindColonyOSContainers {
		t.Errorf("Schema().Kind = %q", schema.Kind)
	}
	if !schema.SeesWorkload {
		t.Error("Schema().SeesWorkload = false, want true — the ColonyOS server has the queue")
	}
}

func TestObserveCountsRunningContainersPerTier(t *testing.T) {
	s := newStack(t, nil)
	s.docker.
		withContainer("executor-storhall-local-1", "local", "running", 1).
		withContainer("executor-storhall-local-2", "local", "running", 2).
		withContainer("executor-storhall-local-3", "local", "created", 3).
		withContainer("executor-storhall-cloud-1", "cloud", "running", 4)

	got, err := colonycontainers.New().Observe(at(now), s.target)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Capacity.LocalReady != 2 {
		t.Errorf("LocalReady = %d, want 2", got.Capacity.LocalReady)
	}
	// A created-but-not-running container is capacity on its way, exactly like
	// a pod that has not become Ready.
	if got.Capacity.LocalPending != 1 {
		t.Errorf("LocalPending = %d, want 1", got.Capacity.LocalPending)
	}
	if got.Capacity.CloudReady != 1 {
		t.Errorf("CloudReady = %d, want 1", got.Capacity.CloudReady)
	}
}

func TestObserveAlsoReadsTheColonyQueue(t *testing.T) {
	s := newStack(t, nil)
	s.colonies.Waiting = []colonyos.Process{
		colonytest.Job("a", 100, now.Add(-time.Minute), "bemis-storhall",
			map[string]string{"exec_seconds": "20"}),
	}

	got, err := colonycontainers.New().Observe(at(now), s.target)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Workload == nil || got.Workload.Queues[100].Depth != 1 {
		t.Errorf("Workload = %+v, want one job waiting at P100", got.Workload)
	}
}

// Containers belonging to something else on the same host must not be counted
// as this target's capacity, or the autoscaler will believe it has executors
// it cannot control.
func TestContainersOfOtherTargetsAreIgnored(t *testing.T) {
	s := newStack(t, nil)
	s.docker.withContainer("executor-storhall-local-1", "local", "running", 1)

	// A container with the right tier but a different target.
	s.docker.mu.Lock()
	s.docker.nextID++
	s.docker.containers["other"] = &fakeContainer{
		id: "other", name: "executor-kvarnberg-local-1", state: "running",
		labels: map[string]string{"autoscaler.target": "kvarnberg", "autoscaler.tier": "local"},
	}
	s.docker.mu.Unlock()

	got, err := colonycontainers.New().Observe(at(now), s.target)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}
	if got.Capacity.LocalReady != 1 {
		t.Errorf("LocalReady = %d, want 1 — the other target's container was counted",
			got.Capacity.LocalReady)
	}
}

func TestApplyStartsTheContainersThePlanAsksFor(t *testing.T) {
	s := newStack(t, nil)

	got, err := colonycontainers.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 3})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if s.docker.running("local") != 3 {
		t.Errorf("running local containers = %d, want 3", s.docker.running("local"))
	}
	if !got.Changed {
		t.Error("Changed = false, want true")
	}
}

// Created is not enough: a container that exists but was never started does no
// work, and reporting it as capacity would stall the queue silently.
func TestEveryCreatedContainerIsAlsoStarted(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 2}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if !s.docker.sawRequest("/start") {
		t.Error("containers were created but never started")
	}
}

func TestTheContainersCarryTheColonyOSConfiguration(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	env := strings.Join(s.docker.anyEnv(), "\n")
	for _, want := range []string{
		"COLONIES_COLONY_NAME=dev",
		"COLONIES_EXECUTOR_TYPE=bemis-storhall",
		"COLONIES_PRVKEY=" + executorKey,
		"COLONIES_EXECUTOR_NAME=",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("container environment is missing %q:\n%s", want, env)
		}
	}
}

// Each executor registers itself with the colony by name. Two containers
// sharing a name would collide on registration.
func TestEachContainerGetsItsOwnExecutorName(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 3}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	names := map[string]bool{}
	for _, container := range []string{
		"executor-storhall-local-1", "executor-storhall-local-2", "executor-storhall-local-3",
	} {
		env := strings.Join(s.docker.envOf(container), "\n")
		for _, line := range strings.Split(env, "\n") {
			if strings.HasPrefix(line, "COLONIES_EXECUTOR_NAME=") {
				if names[line] {
					t.Errorf("two containers share %q", line)
				}
				names[line] = true
			}
		}
	}
	if len(names) != 3 {
		t.Errorf("found %d distinct executor names, want 3", len(names))
	}
}

func TestScalingDownStopsAndRemovesTheSurplus(t *testing.T) {
	s := newStack(t, nil)
	s.docker.
		withContainer("executor-storhall-local-1", "local", "running", 1).
		withContainer("executor-storhall-local-2", "local", "running", 2).
		withContainer("executor-storhall-local-3", "local", "running", 3)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if s.docker.running("local") != 1 {
		t.Errorf("running local containers = %d, want 1", s.docker.running("local"))
	}
	// Stopped is not enough: a stopped container still holds its name, and the
	// next scale-up would collide with it.
	if !s.docker.sawRequest("DELETE") {
		t.Error("surplus containers were stopped but not removed")
	}
}

func TestScalingDownRemovesTheNewestContainersFirst(t *testing.T) {
	s := newStack(t, nil)
	s.docker.
		withContainer("executor-storhall-local-1", "local", "running", 100).
		withContainer("executor-storhall-local-2", "local", "running", 200)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	// The older container has finished starting and may be mid-job; the newer
	// one has done the least work and costs the least to discard.
	got := s.docker.names("local")
	if len(got) != 1 || got[0] != "executor-storhall-local-1" {
		t.Errorf("surviving containers = %v, want only the older executor-storhall-local-1", got)
	}
}

func TestApplyingTheSamePlanTwiceChangesNothing(t *testing.T) {
	s := newStack(t, nil)
	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 2}); err != nil {
		t.Fatalf("first Apply() = %v", err)
	}

	got, err := colonycontainers.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 2})
	if err != nil {
		t.Fatalf("second Apply() = %v", err)
	}

	if got.Changed {
		t.Error("Changed = true for a plan that was already in force")
	}
	if s.docker.running("local") != 2 {
		t.Errorf("running local containers = %d, want the same 2", s.docker.running("local"))
	}
}

func TestARefusedContainerCreationIsReported(t *testing.T) {
	s := newStack(t, nil)
	s.docker.failNextCreate = true

	_, err := colonycontainers.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 1})
	if err == nil {
		t.Fatal("Apply() = nil error for a refused create")
	}
	if !strings.Contains(err.Error(), "no such image") {
		t.Errorf("Apply() = %q, want Docker's own message preserved", err)
	}
}

func TestTheImageIsPulledBeforeTheFirstContainerIsCreated(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonycontainers.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	// A Docker host has no image controller pulling on its behalf, so an
	// unpulled image means every create fails.
	if len(s.docker.pulled) == 0 {
		t.Error("the executor image was never pulled")
	}
}

func TestValidateReachesBothTheDockerHostAndTheColony(t *testing.T) {
	s := newStack(t, nil)

	if err := colonycontainers.New().Validate(context.Background(), s.target); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestValidateRefusesADockerHostItCannotReach(t *testing.T) {
	s := newStack(t, map[string]string{"docker_host": "unix:///nonexistent/docker.sock"})

	err := colonycontainers.New().Validate(context.Background(), s.target)
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Errorf("Validate() = %v, want a refusal naming the Docker host", err)
	}
}

func TestValidateNamesEverySettingThatIsMissing(t *testing.T) {
	err := colonycontainers.New().Validate(context.Background(),
		platform.Target{ID: "empty", Kind: platform.KindColonyOSContainers})

	if err == nil {
		t.Fatal("Validate() = nil for an empty target")
	}
	for _, want := range []string{"docker_host", "executor_image", "colonies_host", "colony_name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q, want it to mention %q", err, want)
		}
	}
}

func TestAnUnsupportedDockerHostSchemeIsRefused(t *testing.T) {
	s := newStack(t, map[string]string{"docker_host": "ssh://somewhere"})

	_, err := colonycontainers.New().Observe(at(now), s.target)
	if err == nil || !strings.Contains(err.Error(), "ssh") {
		t.Errorf("Observe() = %v, want the unsupported scheme named", err)
	}
}
