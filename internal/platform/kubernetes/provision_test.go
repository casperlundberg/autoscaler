package kubernetes_test

import (
	"context"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes"
)

func clientAgainst(t *testing.T, api *fakeAPI) *kubernetes.Client {
	t.Helper()
	server := api.start()

	client, err := kubernetes.NewClient(kubernetes.ClientOptions{
		APIServer: server.URL, Token: api.token, Namespace: "mining",
	})
	if err != nil {
		t.Fatalf("NewClient() = %v", err)
	}
	return client
}

func executorSpec() kubernetes.DeploymentSpec {
	return kubernetes.DeploymentSpec{
		Name:     "executor-storhall-local",
		Image:    "ghcr.io/example/colonyos-executor:v1",
		Replicas: 0,
		Labels: map[string]string{
			"app": "colonyos-executor", "tier": "local", "target": "storhall",
		},
		Env: map[string]string{
			"COLONIES_SERVER_HOST":   "colonies.example.org",
			"COLONIES_EXECUTOR_TYPE": "bemis-storhall",
		},
		SecretEnv: map[string]kubernetes.SecretKeyRef{
			"COLONIES_PRVKEY": {Secret: "colonies-storhall", Key: "prvkey"},
		},
		ImagePullSecrets: []string{"registry-credentials"},
		CPURequest:       "500m",
		MemoryRequest:    "512Mi",
	}
}

// This is what "autonomous provisioning" actually requires: given credentials
// and an image, the autoscaler can bring an executor pool into existence, not
// only resize one somebody else created.
func TestEnsureDeploymentCreatesAPoolThatDoesNotExistYet(t *testing.T) {
	api := newFakeAPI(t)
	client := clientAgainst(t, api)

	created, err := client.EnsureDeployment(context.Background(), executorSpec())
	if err != nil {
		t.Fatalf("EnsureDeployment() = %v", err)
	}
	if !created {
		t.Error("created = false, want true for a Deployment that did not exist")
	}
	if api.replicas("executor-storhall-local") != 0 {
		t.Errorf("replicas = %d, want a pool created at 0 and scaled up by a decision",
			api.replicas("executor-storhall-local"))
	}
}

// The caller decides how big a new pool starts. The adapter creates it at
// zero and lets the first decision size it, but the client itself must honour
// whatever it is given.
func TestANewPoolIsCreatedAtTheRequestedSize(t *testing.T) {
	api := newFakeAPI(t)
	spec := executorSpec()
	spec.Replicas = 7

	if _, err := clientAgainst(t, api).EnsureDeployment(context.Background(), spec); err != nil {
		t.Fatalf("EnsureDeployment() = %v", err)
	}

	if got := api.replicas("executor-storhall-local"); got != 7 {
		t.Errorf("replicas = %d, want the requested 7", got)
	}
}

func TestEnsureDeploymentLeavesAnExistingPoolAlone(t *testing.T) {
	api := newFakeAPI(t).
		withDeployment("executor-storhall-local", 12, map[string]string{"app": "colonyos-executor"})
	client := clientAgainst(t, api)

	created, err := client.EnsureDeployment(context.Background(), executorSpec())
	if err != nil {
		t.Fatalf("EnsureDeployment() = %v", err)
	}

	if created {
		t.Error("created = true for a Deployment that already existed")
	}
	// Re-creating would reset the replica count and roll every running pod,
	// in the middle of whatever burst the pool is serving.
	if got := api.replicas("executor-storhall-local"); got != 12 {
		t.Errorf("replicas = %d, want the existing 12 untouched", got)
	}
}

func TestTheCreatedPodSpecCarriesEverythingAnExecutorNeeds(t *testing.T) {
	api := newFakeAPI(t)
	if _, err := clientAgainst(t, api).EnsureDeployment(context.Background(), executorSpec()); err != nil {
		t.Fatalf("EnsureDeployment() = %v", err)
	}

	body := api.createdDeployment("executor-storhall-local")
	if body == "" {
		t.Fatal("no Deployment was posted")
	}
	for _, want := range []string{
		"ghcr.io/example/colonyos-executor:v1",
		"COLONIES_EXECUTOR_TYPE",
		"bemis-storhall",
		"registry-credentials",
		"500m",
		"512Mi",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("posted Deployment is missing %q:\n%s", want, body)
		}
	}
}

// A private key written as a literal env var is readable by anyone who can
// read a pod spec. It goes in a Secret and is referenced, so the key is
// visible only to whoever can read Secrets in that namespace.
func TestThePrivateKeyIsReferencedFromASecretNotInlined(t *testing.T) {
	api := newFakeAPI(t)
	if _, err := clientAgainst(t, api).EnsureDeployment(context.Background(), executorSpec()); err != nil {
		t.Fatalf("EnsureDeployment() = %v", err)
	}

	body := api.createdDeployment("executor-storhall-local")
	if !strings.Contains(body, "secretKeyRef") {
		t.Errorf("posted Deployment does not reference a Secret:\n%s", body)
	}
	if !strings.Contains(body, "colonies-storhall") {
		t.Errorf("posted Deployment does not name the Secret:\n%s", body)
	}
}

func TestEnsureSecretCreatesTheKeyMaterialItReferences(t *testing.T) {
	api := newFakeAPI(t)

	if err := clientAgainst(t, api).EnsureSecret(context.Background(), "colonies-storhall",
		map[string]string{"prvkey": "fcc79953"}); err != nil {
		t.Fatalf("EnsureSecret() = %v", err)
	}

	if got := api.secretValue("colonies-storhall", "prvkey"); got != "fcc79953" {
		t.Errorf("stored secret value = %q, want the key material", got)
	}
}

func TestEnsureSecretUpdatesAKeyThatHasBeenRotated(t *testing.T) {
	api := newFakeAPI(t)
	client := clientAgainst(t, api)

	if err := client.EnsureSecret(context.Background(), "colonies-storhall",
		map[string]string{"prvkey": "original"}); err != nil {
		t.Fatalf("first EnsureSecret() = %v", err)
	}
	if err := client.EnsureSecret(context.Background(), "colonies-storhall",
		map[string]string{"prvkey": "rotated"}); err != nil {
		t.Fatalf("second EnsureSecret() = %v", err)
	}

	if got := api.secretValue("colonies-storhall", "prvkey"); got != "rotated" {
		t.Errorf("stored secret value = %q, want the rotated key", got)
	}
}

func TestARefusedDeploymentCreationIsReported(t *testing.T) {
	api := newFakeAPI(t)
	api.failNextCreate = true

	_, err := clientAgainst(t, api).EnsureDeployment(context.Background(), executorSpec())
	if err == nil {
		t.Fatal("EnsureDeployment() = nil error for a refused create")
	}
	if !strings.Contains(err.Error(), "executor-storhall-local") {
		t.Errorf("EnsureDeployment() = %q, want it to name the Deployment", err)
	}
}

func TestADeploymentSpecWithNoImageIsRefusedBeforeAnyCall(t *testing.T) {
	spec := executorSpec()
	spec.Image = ""

	_, err := clientAgainst(t, newFakeAPI(t)).EnsureDeployment(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("EnsureDeployment() = %v, want a complaint about the missing image", err)
	}
}
