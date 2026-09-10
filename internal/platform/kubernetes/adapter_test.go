package kubernetes_test

import (
	"context"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes/kubetest"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

func targetFor(apiServer string, overrides map[string]string) platform.Target {
	config := map[string]string{
		"api_server":              apiServer,
		"namespace":               "mining",
		"local_deployment":        "executor-local",
		"cloud_deployment":        "executor-cloud",
		"request_timeout_seconds": "5",
	}
	for k, v := range overrides {
		if v == "" {
			delete(config, k)
			continue
		}
		config[k] = v
	}
	return platform.Target{
		ID:          "storhall",
		Name:        "Storhall",
		Kind:        platform.KindKubernetes,
		Config:      config,
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "test-token"}),
	}
}

func TestTheAdapterAnnouncesThatItCannotSeeAQueue(t *testing.T) {
	schema := kubernetes.New().Schema()

	// Kubernetes knows how many pods are running and nothing about the jobs
	// waiting for them. Saying so is what tells the control loop this target
	// has to be driven rather than polled.
	if schema.SeesWorkload {
		t.Error("Schema().SeesWorkload = true, want false")
	}
	if schema.Kind != platform.KindKubernetes {
		t.Errorf("Schema().Kind = %q", schema.Kind)
	}
}

func TestObserveCountsReadyPodsAndTreatsTheRestAsPending(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 5, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 2, map[string]string{"app": "executor-cloud"}).
		WithPods(map[string]string{"app": "executor-local"}, 3, 2).
		WithPods(map[string]string{"app": "executor-cloud"}, 2, 0)
	server := api.Start()

	got, err := kubernetes.New().Observe(context.Background(), targetFor(server.URL, nil))
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	want := domain.Capacity{LocalReady: 3, LocalPending: 2, CloudReady: 2, CloudPending: 0}
	if got.Capacity != want {
		t.Errorf("Capacity = %+v, want %+v", got.Capacity, want)
	}
	if got.Workload != nil {
		t.Errorf("Workload = %+v, want nil", got.Workload)
	}
}

// A pod that exists but is not Ready is capacity that has been asked for and
// has not arrived. Counting it as usable is what makes a controller believe it
// has more throughput than it does.
func TestPodsThatAreNotReadyAreNotCountedAsCapacity(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 4, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"}).
		WithPods(map[string]string{"app": "executor-local"}, 1, 3)
	server := api.Start()

	got, err := kubernetes.New().Observe(context.Background(), targetFor(server.URL, nil))
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Capacity.LocalReady != 1 {
		t.Errorf("LocalReady = %d, want 1", got.Capacity.LocalReady)
	}
	if got.Capacity.LocalPending != 3 {
		t.Errorf("LocalPending = %d, want 3", got.Capacity.LocalPending)
	}
}

// Pending is derived from the Deployment's own desired count, not from
// counting non-ready pods: during a scale-up the pods do not exist yet, and a
// controller that could not see the request it already made would issue it
// again every cycle.
func TestPendingIncludesReplicasTheSchedulerHasNotCreatedYet(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 10, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"}).
		WithPods(map[string]string{"app": "executor-local"}, 2, 0)
	server := api.Start()

	got, err := kubernetes.New().Observe(context.Background(), targetFor(server.URL, nil))
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Capacity.LocalReady != 2 || got.Capacity.LocalPending != 8 {
		t.Errorf("Capacity = %+v, want 2 ready and 8 pending", got.Capacity)
	}
}

func TestMoreReadyPodsThanReplicasNeverProducesNegativePending(t *testing.T) {
	// Happens for real mid-scale-down, while terminating pods are still Ready.
	api := kubetest.New(t).
		WithDeployment("executor-local", 1, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"}).
		WithPods(map[string]string{"app": "executor-local"}, 4, 0)
	server := api.Start()

	got, err := kubernetes.New().Observe(context.Background(), targetFor(server.URL, nil))
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}

	if got.Capacity.LocalPending < 0 {
		t.Errorf("LocalPending = %d, want never negative", got.Capacity.LocalPending)
	}
}

func TestApplyScalesBothDeployments(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 2, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"})
	server := api.Start()

	got, err := kubernetes.New().Apply(context.Background(), targetFor(server.URL, nil),
		domain.Plan{LocalExecutors: 8, CloudExecutors: 3})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if api.Replicas("executor-local") != 8 {
		t.Errorf("local replicas = %d, want 8", api.Replicas("executor-local"))
	}
	if api.Replicas("executor-cloud") != 3 {
		t.Errorf("cloud replicas = %d, want 3", api.Replicas("executor-cloud"))
	}
	if !got.Changed {
		t.Error("Changed = false, want true")
	}
	if want := (domain.Plan{LocalExecutors: 8, CloudExecutors: 3}); got.Applied != want {
		t.Errorf("Applied = %+v, want %+v", got.Applied, want)
	}
}

// The scale subresource touches replicas and nothing else. Patching the
// Deployment itself risks writing the pod template, which rolls the whole pool
// in the middle of a burst.
func TestApplyUsesTheScaleSubresource(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 2, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"})
	server := api.Start()

	if _, err := kubernetes.New().Apply(context.Background(), targetFor(server.URL, nil),
		domain.Plan{LocalExecutors: 3}); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if !api.SawRequest("PATCH /apis/apps/v1/namespaces/mining/deployments/executor-local/scale") {
		t.Error("no PATCH to the scale subresource was sent")
	}
}

func TestApplySkipsATierThatIsAlreadyWhereItShouldBe(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 4, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"})
	server := api.Start()

	got, err := kubernetes.New().Apply(context.Background(), targetFor(server.URL, nil),
		domain.Plan{LocalExecutors: 4, CloudExecutors: 0})
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if got.Changed {
		t.Error("Changed = true when both tiers already matched the plan")
	}
	if api.SawRequest("PATCH") {
		t.Error("a write was sent for a plan that changed nothing")
	}
}

// A rejected write must surface. Reporting success would leave the control
// loop believing capacity it never got, and the next cycle would compute from
// a fiction.
func TestARejectedScaleIsReportedAsAnError(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 2, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"})
	api.FailNextPatch = true
	server := api.Start()

	_, err := kubernetes.New().Apply(context.Background(), targetFor(server.URL, nil),
		domain.Plan{LocalExecutors: 8})
	if err == nil {
		t.Fatal("Apply() = nil error for a rejected scale")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("Apply() = %q, want the API server's own reason preserved", err)
	}
}

func TestABadTokenIsReportedAsAnAuthenticationProblem(t *testing.T) {
	api := kubetest.New(t).WithDeployment("executor-local", 1, map[string]string{"app": "x"})
	server := api.Start()

	target := targetFor(server.URL, nil)
	target.Credentials = secret.NewBundle(map[string]string{"bearer_token": "wrong"})

	_, err := kubernetes.New().Observe(context.Background(), target)
	if err == nil {
		t.Fatal("Observe() = nil error with a bad token")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "401") &&
		!strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
		t.Errorf("Observe() = %q, want it to point at authentication", err)
	}
}

func TestAMissingDeploymentIsReportedByName(t *testing.T) {
	server := kubetest.New(t).Start()

	_, err := kubernetes.New().Observe(context.Background(), targetFor(server.URL, nil))
	if err == nil {
		t.Fatal("Observe() = nil error for a deployment that does not exist")
	}
	if !strings.Contains(err.Error(), "executor-local") {
		t.Errorf("Observe() = %q, want it to name the missing deployment", err)
	}
}

func TestATargetWithNoCloudDeploymentIsLocalOnly(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 3, map[string]string{"app": "executor-local"}).
		WithPods(map[string]string{"app": "executor-local"}, 3, 0)
	server := api.Start()
	target := targetFor(server.URL, map[string]string{"cloud_deployment": ""})

	got, err := kubernetes.New().Observe(context.Background(), target)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}
	if got.Capacity.CloudReady != 0 || got.Capacity.CloudPending != 0 {
		t.Errorf("Capacity = %+v, want no cloud capacity", got.Capacity)
	}
}

func TestAskingALocalOnlyTargetForCloudCapacityIsRefusedClearly(t *testing.T) {
	api := kubetest.New(t).WithDeployment("executor-local", 3, map[string]string{"app": "executor-local"})
	server := api.Start()
	target := targetFor(server.URL, map[string]string{"cloud_deployment": ""})

	_, err := kubernetes.New().Apply(context.Background(), target, domain.Plan{CloudExecutors: 2})
	if err == nil {
		t.Fatal("Apply() = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "cloud_deployment") {
		t.Errorf("Apply() = %q, want it to name the setting that is missing", err)
	}
}

// Registration is the moment to find out that a token is wrong or a name is
// misspelled — not three in the morning during a burst.
func TestValidateProvesTheCredentialsAndNamesActuallyWork(t *testing.T) {
	api := kubetest.New(t).
		WithDeployment("executor-local", 1, map[string]string{"app": "executor-local"}).
		WithDeployment("executor-cloud", 0, map[string]string{"app": "executor-cloud"})
	server := api.Start()

	if err := kubernetes.New().Validate(context.Background(), targetFor(server.URL, nil)); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsATargetWhoseDeploymentDoesNotExist(t *testing.T) {
	api := kubetest.New(t).WithDeployment("executor-local", 1, map[string]string{"app": "executor-local"})
	server := api.Start()

	err := kubernetes.New().Validate(context.Background(), targetFor(server.URL, nil))
	if err == nil || !strings.Contains(err.Error(), "executor-cloud") {
		t.Errorf("Validate() = %v, want the missing cloud deployment named", err)
	}
}

func TestValidateRejectsATargetMissingRequiredSettings(t *testing.T) {
	err := kubernetes.New().Validate(context.Background(), platform.Target{Kind: platform.KindKubernetes})

	if err == nil {
		t.Fatal("Validate() = nil for an empty target")
	}
	for _, want := range []string{"namespace", "local_deployment", "bearer_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q, want it to mention %q", err, want)
		}
	}
}

func TestApplyRefusesANegativeReplicaCount(t *testing.T) {
	api := kubetest.New(t).WithDeployment("executor-local", 1, map[string]string{"app": "executor-local"})
	server := api.Start()

	_, err := kubernetes.New().Apply(context.Background(), targetFor(server.URL, nil),
		domain.Plan{LocalExecutors: -1})
	if err == nil {
		t.Fatal("Apply() = nil error for a negative count")
	}
}
