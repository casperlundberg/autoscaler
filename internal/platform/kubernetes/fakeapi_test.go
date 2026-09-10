package kubernetes_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is a small but honest stand-in for the Kubernetes API server: it
// speaks the real paths, the real JSON shapes and the real status codes.
// Testing the adapter against it exercises the request building, the label
// selector, the JSON decoding and the error handling — everything a mocked
// client would have quietly skipped.
type fakeAPI struct {
	mu sync.Mutex
	t  *testing.T

	token       string
	deployments map[string]*fakeDeployment
	pods        []fakePod

	// requests records what the adapter actually sent, so a test can assert
	// on the wire and not only on the outcome.
	requests []string

	// failNextPatch makes the next scale request fail, to check that a
	// rejected apply is reported rather than assumed to have worked.
	failNextPatch bool
}

type fakeDeployment struct {
	replicas int
	selector map[string]string
}

type fakePod struct {
	labels      map[string]string
	ready       bool
	terminating bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{t: t, token: "test-token", deployments: map[string]*fakeDeployment{}}
}

func (f *fakeAPI) withDeployment(name string, replicas int, selector map[string]string) *fakeAPI {
	f.deployments[name] = &fakeDeployment{replicas: replicas, selector: selector}
	return f
}

func (f *fakeAPI) withPods(labels map[string]string, ready, pending int) *fakeAPI {
	for i := 0; i < ready; i++ {
		f.pods = append(f.pods, fakePod{labels: labels, ready: true})
	}
	for i := 0; i < pending; i++ {
		f.pods = append(f.pods, fakePod{labels: labels, ready: false})
	}
	return f
}

func (f *fakeAPI) replicas(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.deployments[name]; ok {
		return d.replicas
	}
	return -1
}

func (f *fakeAPI) sawRequest(substring string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, substring) {
			return true
		}
	}
	return false
}

func (f *fakeAPI) start() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(server.Close)
	return server
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		writeStatus(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	switch {
	case strings.HasSuffix(r.URL.Path, "/scale") && r.Method == http.MethodPatch:
		f.serveScale(w, r)
	case strings.Contains(r.URL.Path, "/deployments/") && r.Method == http.MethodGet:
		f.serveDeployment(w, r)
	case strings.HasSuffix(r.URL.Path, "/pods") && r.Method == http.MethodGet:
		f.servePods(w, r)
	default:
		writeStatus(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	}
}

func (f *fakeAPI) serveDeployment(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := lastSegment(r.URL.Path)
	d, ok := f.deployments[name]
	if !ok {
		writeStatus(w, http.StatusNotFound, `deployments.apps "`+name+`" not found`)
		return
	}
	writeJSON(w, map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"replicas": d.replicas,
			"selector": map[string]any{"matchLabels": d.selector},
		},
	})
}

func (f *fakeAPI) serveScale(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failNextPatch {
		f.failNextPatch = false
		writeStatus(w, http.StatusForbidden, "deployments.apps is forbidden")
		return
	}

	name := lastSegment(strings.TrimSuffix(r.URL.Path, "/scale"))
	d, ok := f.deployments[name]
	if !ok {
		writeStatus(w, http.StatusNotFound, `deployments.apps "`+name+`" not found`)
		return
	}

	var patch struct {
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil || patch.Spec.Replicas == nil {
		writeStatus(w, http.StatusBadRequest, "malformed scale patch")
		return
	}
	d.replicas = *patch.Spec.Replicas

	writeJSON(w, map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"replicas": d.replicas},
	})
}

func (f *fakeAPI) servePods(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	selector := parseSelector(r.URL.Query().Get("labelSelector"))

	items := []any{}
	for _, p := range f.pods {
		if !matches(p.labels, selector) {
			continue
		}
		metadata := map[string]any{"name": "pod"}
		if p.terminating {
			metadata["deletionTimestamp"] = "2026-09-10T12:00:00Z"
		}
		status := "False"
		if p.ready {
			status = "True"
		}
		items = append(items, map[string]any{
			"metadata": metadata,
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": status}},
			},
		})
	}
	writeJSON(w, map[string]any{"items": items})
}

func parseSelector(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		if found {
			out[key] = value
		}
	}
	return out
}

func matches(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func lastSegment(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1]
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeStatus(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "status": "Failure", "message": message, "code": code,
	})
}
