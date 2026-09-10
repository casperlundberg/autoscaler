package kubernetes_test

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes"
)

// inCluster stands up a TLS API server with its own CA and a service account
// directory laid out exactly as Kubernetes mounts one, so the in-cluster path
// is exercised end to end: reading the mounted files, trusting the cluster CA,
// and finding the API server through the environment.
func inCluster(t *testing.T, api *fakeAPI, files map[string]string) (*kubernetes.Adapter, func(string) string) {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(api.serve))
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	})

	// files overrides the mounted content; an empty override means "do not
	// mount this file at all", which is how a broken mount is simulated.
	dir := t.TempDir()
	mounted := map[string]string{
		"token":     api.token,
		"ca.crt":    string(caPEM),
		"namespace": "mining",
	}
	for name, override := range files {
		if override == "" {
			delete(mounted, name)
			continue
		}
		mounted[name] = override
	}
	for name, content := range mounted {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing server URL: %v", err)
	}
	env := func(key string) string {
		switch key {
		case "KUBERNETES_SERVICE_HOST":
			return parsed.Hostname()
		case "KUBERNETES_SERVICE_PORT":
			return parsed.Port()
		}
		return ""
	}

	return kubernetes.NewWithServiceAccount(dir, env), env
}

func inClusterTarget() platform.Target {
	return platform.Target{
		ID:   "storhall",
		Kind: platform.KindKubernetes,
		Config: map[string]string{
			"in_cluster":       "true",
			"namespace":        "mining",
			"local_deployment": "executor-local",
		},
	}
}

func TestTheAdapterCanRunOnTheClusterItScales(t *testing.T) {
	api := newFakeAPI(t).
		withDeployment("executor-local", 3, map[string]string{"app": "executor-local"}).
		withPods(map[string]string{"app": "executor-local"}, 3, 0)
	adapter, _ := inCluster(t, api, nil)

	got, err := adapter.Observe(context.Background(), inClusterTarget())
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}
	if got.Capacity.LocalReady != 3 {
		t.Errorf("LocalReady = %d, want 3", got.Capacity.LocalReady)
	}
}

func TestInClusterNeedsNoBearerTokenInTheTarget(t *testing.T) {
	api := newFakeAPI(t).withDeployment("executor-local", 1, map[string]string{"app": "executor-local"})
	adapter, _ := inCluster(t, api, nil)

	// The whole point: the operator grants a Role instead of storing a token
	// in this service, so the target carries no credentials at all.
	if err := adapter.Validate(context.Background(), inClusterTarget()); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestAMissingServiceAccountTokenFailsLoudlyWithThePath(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := inCluster(t, api, map[string]string{"token": ""})

	_, err := adapter.Observe(context.Background(), inClusterTarget())
	if err == nil {
		t.Fatal("Observe() = nil error with no token mounted")
	}
	// Degrading to an unauthenticated client here would fail later and far
	// less legibly, so this must name the file that is missing.
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("Observe() = %q, want it to name the missing token file", err)
	}
}

func TestWithoutTheKubernetesEnvironmentTheAPIServerCannotBeFound(t *testing.T) {
	api := newFakeAPI(t)
	dir := t.TempDir()
	for name, content := range map[string]string{"token": "t", "ca.crt": "", "namespace": "mining"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	_ = api

	adapter := kubernetes.NewWithServiceAccount(dir, func(string) string { return "" })

	_, err := adapter.Observe(context.Background(), inClusterTarget())
	if err == nil || !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Errorf("Observe() = %v, want it to name the missing environment", err)
	}
}
