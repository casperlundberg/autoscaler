package colonypods_test

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos/colonytest"
	"github.com/casperlundberg/autoscaler/internal/platform/colonypods"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes/kubetest"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

const (
	executorKey = "fcc79953d8a751bf41db661592dc34d30004b1a651ffa0725b03ac227641499d"
	executorID  = "039231c7644e04b6895471dd5335cf332681c54e27f81fac54f9067b3f2c0103"
)

var now = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// stack stands up both back ends a ColonyOS-on-Kubernetes target depends on:
// a Kubernetes API server that provisions the pods, and a ColonyOS server that
// owns the queue those pods serve.
type stack struct {
	kube     *kubetest.Server
	colonies *colonytest.Server
	target   platform.Target
}

func newStack(t *testing.T, overrides map[string]string) *stack {
	t.Helper()

	kube := kubetest.New(t)
	colonies := colonytest.New(t, executorID)
	kubeServer := kube.Start()
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
		"api_server":     kubeServer.URL,
		"namespace":      "mining",
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
		kube:     kube,
		colonies: colonies,
		target: platform.Target{
			ID: "storhall", Name: "Storhall", Kind: platform.KindColonyOSPods,
			Config: config,
			Credentials: secret.NewBundle(map[string]string{
				"bearer_token":    kube.Token,
				"colonies_prvkey": executorKey,
			}),
		},
	}
}

func at(when time.Time) context.Context {
	return platform.WithCycleTime(context.Background(), when)
}

func TestThisAdapterCanSeeItsOwnQueue(t *testing.T) {
	schema := colonypods.New().Schema()

	// Unlike plain Kubernetes, this platform knows what the pods are for: the
	// ColonyOS server holds the queue they serve, so a target here can be
	// polled rather than driven.
	if !schema.SeesWorkload {
		t.Error("Schema().SeesWorkload = false, want true")
	}
	if schema.Kind != platform.KindColonyOSPods {
		t.Errorf("Schema().Kind = %q", schema.Kind)
	}
}

func TestObserveReportsBothTheQueueAndTheRunningPods(t *testing.T) {
	s := newStack(t, nil)
	s.kube.
		WithDeployment("executor-storhall-local", 4, map[string]string{"app": "colonyos-executor", "tier": "local"}).
		WithPods(map[string]string{"app": "colonyos-executor", "tier": "local"}, 3, 0)
	s.colonies.Waiting = []colonyos.Process{
		colonytest.Job("a", 100, now.Add(-time.Minute), "bemis-storhall",
			map[string]string{"exec_seconds": "20"}),
		colonytest.Job("b", 100, now.Add(-2*time.Minute), "bemis-storhall", nil),
	}

	got, err := colonypods.New().Observe(at(now), s.target)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Capacity.LocalReady != 3 || got.Capacity.LocalPending != 1 {
		t.Errorf("Capacity = %+v, want 3 ready and 1 pending", got.Capacity)
	}
	if got.Workload == nil {
		t.Fatal("Workload = nil, want the ColonyOS queue")
	}
	if got.Workload.Queues[100].Depth != 2 {
		t.Errorf("P100 depth = %d, want 2", got.Workload.Queues[100].Depth)
	}
	if got.Workload.ExecutorThroughput <= 0 {
		t.Errorf("ExecutorThroughput = %v, want positive", got.Workload.ExecutorThroughput)
	}
}

// A pool that has not been created yet is not an error — it is the normal
// state before the first scale-up, and the whole reason this adapter can
// provision from nothing.
func TestAPoolThatDoesNotExistYetReportsNoCapacity(t *testing.T) {
	s := newStack(t, nil)

	got, err := colonypods.New().Observe(at(now), s.target)
	if err != nil {
		t.Fatalf("Observe() = %v, want a missing pool treated as empty", err)
	}
	if got.Capacity.Total() != 0 {
		t.Errorf("Capacity = %+v, want nothing", got.Capacity)
	}
}

func TestApplyCreatesThePoolItNeedsAndSizesIt(t *testing.T) {
	s := newStack(t, nil)

	got, err := colonypods.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 5})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if s.kube.Replicas("executor-storhall-local") != 5 {
		t.Errorf("local replicas = %d, want 5", s.kube.Replicas("executor-storhall-local"))
	}
	if !got.Changed {
		t.Error("Changed = false, want true")
	}
}

func TestTheExecutorPodsAreGivenEverythingTheyNeedToJoinTheColony(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonypods.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 2}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	body := s.kube.CreatedDeployment("executor-storhall-local")
	for _, want := range []string{
		"COLONIES_SERVER_HOST", "COLONIES_COLONY_NAME", "dev",
		"COLONIES_EXECUTOR_TYPE", "bemis-storhall",
		"COLONIES_PRVKEY", "secretKeyRef",
		"COLONIES_EXECUTOR_NAME", "fieldRef",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the created pool is missing %q:\n%s", want, body)
		}
	}
}

// The executor key is what lets a pod join the colony. It has to reach the
// pods, and it must not be readable in a pod spec.
func TestTheExecutorKeyIsWrittenToASecretBeforeThePoolIsCreated(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonypods.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if got := s.kube.SecretValue("colonies-storhall", "prvkey"); got != executorKey {
		t.Errorf("stored key = %q, want the executor private key", got)
	}
	if body := s.kube.CreatedDeployment("executor-storhall-local"); strings.Contains(body, executorKey) {
		t.Error("the private key was inlined into the pod spec")
	}
}

func TestTheCloudTierIsProvisionedOnlyWhenItIsAskedFor(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonypods.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 3}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if s.kube.CreatedDeployment("executor-storhall-cloud") != "" {
		t.Error("a cloud pool was created for a plan that asked for none")
	}

	if _, err := colonypods.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 3, CloudExecutors: 2}); err != nil {
		t.Fatalf("second Apply() = %v", err)
	}
	if s.kube.Replicas("executor-storhall-cloud") != 2 {
		t.Errorf("cloud replicas = %d, want 2", s.kube.Replicas("executor-storhall-cloud"))
	}
}

func TestTheTiersAreDistinguishedByLabelSoTheirPodsAreCountedSeparately(t *testing.T) {
	s := newStack(t, nil)

	if _, err := colonypods.New().Apply(at(now), s.target,
		domain.Plan{LocalExecutors: 1, CloudExecutors: 1}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	local := s.kube.CreatedDeployment("executor-storhall-local")
	cloud := s.kube.CreatedDeployment("executor-storhall-cloud")
	if !strings.Contains(local, `"tier":"local"`) {
		t.Errorf("local pool is not labelled with its tier:\n%s", local)
	}
	if !strings.Contains(cloud, `"tier":"cloud"`) {
		t.Errorf("cloud pool is not labelled with its tier:\n%s", cloud)
	}
}

func TestDeploymentNamesCanBeOverriddenForAnExistingEstate(t *testing.T) {
	s := newStack(t, map[string]string{
		"local_deployment": "legacy-executors-onprem",
		"cloud_deployment": "legacy-executors-cloud",
	})

	if _, err := colonypods.New().Apply(at(now), s.target, domain.Plan{LocalExecutors: 2}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	if s.kube.Replicas("legacy-executors-onprem") != 2 {
		t.Error("the configured Deployment name was not used")
	}
}

func TestValidateChecksBothBackEnds(t *testing.T) {
	s := newStack(t, nil)

	if err := colonypods.New().Validate(context.Background(), s.target); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsAKeyThatIsNotAColonyMember(t *testing.T) {
	s := newStack(t, nil)

	// Point the target at a colony this key is not a member of.
	other := colonytest.New(t, "somebody-else")
	server := other.Start()
	parsed, _ := url.Parse(server.URL)
	s.target.Config["colonies_host"] = parsed.Hostname()
	s.target.Config["colonies_port"] = parsed.Port()

	err := colonypods.New().Validate(context.Background(), s.target)
	if err == nil || !strings.Contains(err.Error(), "member") {
		t.Errorf("Validate() = %v, want a refusal naming colony membership", err)
	}
}

func TestValidateRejectsAMalformedExecutorKey(t *testing.T) {
	s := newStack(t, nil)
	s.target.Credentials = secret.NewBundle(map[string]string{
		"bearer_token":    s.kube.Token,
		"colonies_prvkey": "not-a-key",
	})

	err := colonypods.New().Validate(context.Background(), s.target)
	if err == nil || !strings.Contains(err.Error(), "colonies_prvkey") {
		t.Errorf("Validate() = %v, want the bad key named", err)
	}
}

func TestValidateNamesEverySettingThatIsMissing(t *testing.T) {
	err := colonypods.New().Validate(context.Background(),
		platform.Target{ID: "empty", Kind: platform.KindColonyOSPods})

	if err == nil {
		t.Fatal("Validate() = nil for an empty target")
	}
	for _, want := range []string{"namespace", "executor_image", "colonies_host", "colony_name", "executor_type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q, want it to mention %q", err, want)
		}
	}
}
