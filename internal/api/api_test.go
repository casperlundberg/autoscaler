package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/api"
	"github.com/casperlundberg/autoscaler/internal/controller"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

type fixture struct {
	server *httptest.Server
	token  string
}

func newFixture(t *testing.T, token string) *fixture {
	t.Helper()

	platforms, err := platform.NewRegistry(simulation.New())
	if err != nil {
		t.Fatalf("platform.NewRegistry() = %v", err)
	}
	reg, err := registry.New(platforms, registry.NewMemoryStore())
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	handler := api.New(api.Options{
		Platforms:  platforms,
		Targets:    reg,
		Controller: controller.New(platforms, reg),
		Token:      token,
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &fixture{server: server, token: token}
}

func (f *fixture) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}

	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return body
}

func simulationTarget() map[string]any {
	return map[string]any{
		"target": map[string]any{
			"id": "run-1", "name": "Simulated run",
			"kind": "simulation", "mode": "driven",
			"config": map[string]string{"local_coldstart_seconds": "0"},
		},
	}
}

func createTarget(t *testing.T, f *fixture) {
	t.Helper()
	if resp := f.do(t, http.MethodPost, "/v1/targets", simulationTarget()); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/targets = %d, want 201: %v", resp.StatusCode, decode(t, resp))
	}
}

func TestHealthAndReadinessAreOpen(t *testing.T) {
	f := newFixture(t, "secret-token")

	for _, path := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+path, nil)
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()

		// A probe cannot carry a credential, so requiring one here means the
		// pod is killed for being unauthenticated rather than unhealthy.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a token = %d, want 200", path, resp.StatusCode)
		}
	}
}

// This service holds Kubernetes tokens, ColonyOS keys and Docker
// certificates. An unauthenticated caller must not be able to list, let alone
// change, anything.
func TestEveryOtherEndpointRequiresTheToken(t *testing.T) {
	f := newFixture(t, "secret-token")

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/targets", nil)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/targets: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/targets without a token = %d, want 401", resp.StatusCode)
	}
}

func TestAWrongTokenIsRejected(t *testing.T) {
	f := newFixture(t, "secret-token")

	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/targets: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a wrong token = %d, want 401", resp.StatusCode)
	}
}

// The platform list is what lets a UI render a registration form for a
// platform it was never written for.
func TestThePlatformListDescribesEveryFieldAndWhichAreSecret(t *testing.T) {
	f := newFixture(t, "")

	resp := f.do(t, http.MethodGet, "/v1/platforms", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/platforms = %d", resp.StatusCode)
	}

	body := decode(t, resp)
	platforms, ok := body["platforms"].([]any)
	if !ok || len(platforms) == 0 {
		t.Fatalf("platforms = %#v, want a non-empty list", body["platforms"])
	}
	first, _ := platforms[0].(map[string]any)
	for _, field := range []string{"kind", "summary", "sees_workload", "config"} {
		if _, ok := first[field]; !ok {
			t.Errorf("platform entry has no %q: %#v", field, first)
		}
	}
}

func TestDefaultSettingsAreServedForANewTargetForm(t *testing.T) {
	f := newFixture(t, "")

	resp := f.do(t, http.MethodGet, "/v1/settings/defaults", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/settings/defaults = %d", resp.StatusCode)
	}
	if _, ok := decode(t, resp)["decision_interval_seconds"]; !ok {
		t.Error("the defaults do not include decision_interval_seconds")
	}
}

func TestATargetCanBeCreatedListedAndFetched(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	list := decode(t, f.do(t, http.MethodGet, "/v1/targets", nil))
	targets, ok := list["targets"].([]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("targets = %#v, want one", list["targets"])
	}

	resp := f.do(t, http.MethodGet, "/v1/targets/run-1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/targets/run-1 = %d", resp.StatusCode)
	}
}

func TestCreatingATargetWithSettingsAppliesThem(t *testing.T) {
	f := newFixture(t, "")
	body := simulationTarget()
	body["settings"] = map[string]any{"local_executor_cap": 7}

	resp := f.do(t, http.MethodPost, "/v1/targets", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/targets = %d: %v", resp.StatusCode, decode(t, resp))
	}

	settings := decode(t, f.do(t, http.MethodGet, "/v1/targets/run-1/settings", nil))
	document, _ := settings["settings"].(map[string]any)
	if document["local_executor_cap"] != 7.0 {
		t.Errorf("local_executor_cap = %v, want 7", document["local_executor_cap"])
	}
	// Fields the caller did not mention keep the shipped defaults rather than
	// becoming zero.
	if document["max_scale_up_step"] == 0.0 {
		t.Error("max_scale_up_step is zero; unmentioned settings were not defaulted")
	}
}

func TestAnUnknownTargetIs404(t *testing.T) {
	f := newFixture(t, "")

	if resp := f.do(t, http.MethodGet, "/v1/targets/nobody", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/targets/nobody = %d, want 404", resp.StatusCode)
	}
}

func TestADuplicateTargetIs409(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	if resp := f.do(t, http.MethodPost, "/v1/targets", simulationTarget()); resp.StatusCode != http.StatusConflict {
		t.Errorf("second POST /v1/targets = %d, want 409", resp.StatusCode)
	}
}

func TestAnUnusableTargetIs400WithAReason(t *testing.T) {
	f := newFixture(t, "")
	body := simulationTarget()
	body["target"].(map[string]any)["id"] = "Not A Valid Id"

	resp := f.do(t, http.MethodPost, "/v1/targets", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /v1/targets = %d, want 400", resp.StatusCode)
	}
	if message, _ := decode(t, resp)["error"].(string); !strings.Contains(message, "id") {
		t.Errorf("error = %q, want it to explain what is wrong", message)
	}
}

// Credentials go in and must never come back.
func TestCredentialsAreRedactedInEveryResponse(t *testing.T) {
	f := newFixture(t, "")
	body := simulationTarget()
	body["target"].(map[string]any)["credentials"] = map[string]string{"bearer_token": "super-secret"}

	created := f.do(t, http.MethodPost, "/v1/targets", body)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/targets = %d: %v", created.StatusCode, decode(t, created))
	}

	for _, path := range []string{"/v1/targets", "/v1/targets/run-1"} {
		resp := f.do(t, http.MethodGet, path, nil)
		var raw bytes.Buffer
		if _, err := raw.ReadFrom(resp.Body); err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(raw.String(), "super-secret") {
			t.Errorf("GET %s leaked a credential:\n%s", path, raw.String())
		}
	}
}

func TestSettingsCanBePatchedOneFieldAtATime(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	resp := f.do(t, http.MethodPatch, "/v1/targets/run-1/settings",
		map[string]any{"local_executor_cap": 42})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings = %d: %v", resp.StatusCode, decode(t, resp))
	}

	body := decode(t, resp)
	if body["version"] != 2.0 {
		t.Errorf("version = %v, want 2", body["version"])
	}
	document, _ := body["settings"].(map[string]any)
	if document["local_executor_cap"] != 42.0 {
		t.Errorf("local_executor_cap = %v, want 42", document["local_executor_cap"])
	}
}

func TestASettingsPatchThatCannotRunIs400(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	resp := f.do(t, http.MethodPatch, "/v1/targets/run-1/settings",
		map[string]any{"safety_factor": 0.1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH settings = %d, want 400", resp.StatusCode)
	}
}

// Two operators editing the same target must not silently overwrite each
// other, so a write may carry the version it was based on.
func TestAStaleSettingsWriteIs409(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	first := f.do(t, http.MethodPatch, "/v1/targets/run-1/settings?expected_version=1",
		map[string]any{"local_executor_cap": 42})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first PATCH = %d", first.StatusCode)
	}

	second := f.do(t, http.MethodPatch, "/v1/targets/run-1/settings?expected_version=1",
		map[string]any{"local_executor_cap": 7})
	if second.StatusCode != http.StatusConflict {
		t.Errorf("stale PATCH = %d, want 409", second.StatusCode)
	}
}

func TestSettingsHistoryShowsWhatChanged(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)
	f.do(t, http.MethodPatch, "/v1/targets/run-1/settings", map[string]any{"local_executor_cap": 42})

	resp := f.do(t, http.MethodGet, "/v1/targets/run-1/settings/history", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET history = %d", resp.StatusCode)
	}
	changes, ok := decode(t, resp)["changes"].([]any)
	if !ok || len(changes) != 1 {
		t.Fatalf("changes = %#v, want one entry", decode(t, resp)["changes"])
	}
}

func TestWarningsAreReturnedAlongsideSettings(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	resp := f.do(t, http.MethodPatch, "/v1/targets/run-1/settings", map[string]any{"dry_run": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings = %d", resp.StatusCode)
	}
	warnings, _ := decode(t, resp)["warnings"].([]any)
	if len(warnings) == 0 {
		t.Error("warnings is empty, want dry_run flagged")
	}
}

// The endpoint the simulation app lives on: hand over a queue, get back what
// the real engine decides about it.
func TestACycleCanBeDrivenWithASuppliedWorkload(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	resp := f.do(t, http.MethodPost, "/v1/targets/run-1/cycle", map[string]any{
		"at": "2026-09-10T12:00:00Z",
		"workload": map[string]any{
			"queues": map[string]any{
				"100": map[string]any{
					"depth": 400, "oldest_job_age_seconds": 50, "arrival_rate_per_second": 2,
				},
			},
			"executor_throughput_per_second": 1,
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST cycle = %d: %v", resp.StatusCode, decode(t, resp))
	}

	body := decode(t, resp)
	decision, ok := body["decision"].(map[string]any)
	if !ok {
		t.Fatalf("decision missing: %#v", body)
	}
	plan, _ := decision["plan"].(map[string]any)
	if plan["local_executors"] == 0.0 && plan["cloud_executors"] == 0.0 {
		t.Errorf("plan = %#v, want capacity for a queue about to breach", plan)
	}
	if decision["reason"] == "" {
		t.Error("reason is empty")
	}
}

func TestACycleWithNoWorkloadOnADrivenTargetIs400(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	resp := f.do(t, http.MethodPost, "/v1/targets/run-1/cycle", map[string]any{
		"at": "2026-09-10T12:00:00Z",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST cycle = %d, want 400", resp.StatusCode)
	}
}

func TestTheStatusViewShowsTheLastDecision(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)
	f.do(t, http.MethodPost, "/v1/targets/run-1/cycle", map[string]any{
		"at": "2026-09-10T12:00:00Z",
		"workload": map[string]any{
			"queues": map[string]any{
				"100": map[string]any{"depth": 400, "oldest_job_age_seconds": 50, "arrival_rate_per_second": 2},
			},
			"executor_throughput_per_second": 1,
		},
	})

	resp := f.do(t, http.MethodGet, "/v1/targets/run-1/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	if _, ok := decode(t, resp)["last_decision"]; !ok {
		t.Error("status has no last_decision")
	}
}

func TestATargetCanBeDeleted(t *testing.T) {
	f := newFixture(t, "")
	createTarget(t, f)

	if resp := f.do(t, http.MethodDelete, "/v1/targets/run-1", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", resp.StatusCode)
	}
	if resp := f.do(t, http.MethodGet, "/v1/targets/run-1", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", resp.StatusCode)
	}
}

func TestAMalformedBodyIs400NotAPanic(t *testing.T) {
	f := newFixture(t, "")

	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/v1/targets",
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with a malformed body = %d, want 400", resp.StatusCode)
	}
}

func TestAnUnknownFieldInATargetIsRefused(t *testing.T) {
	f := newFixture(t, "")
	body := simulationTarget()
	body["targett"] = body["target"]
	delete(body, "target")

	if resp := f.do(t, http.MethodPost, "/v1/targets", body); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with an unknown field = %d, want 400", resp.StatusCode)
	}
}
