package colonycontainers_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeDocker speaks the Docker Engine API over a unix socket, which is how a
// real Docker host is reached. Running the fake on a socket rather than a TCP
// port means the dialing path the adapter uses in production is the one under
// test.
type fakeDocker struct {
	mu sync.Mutex
	t  *testing.T

	containers map[string]*fakeContainer
	nextID     int

	requests []string

	failNextCreate bool
	pulled         []string
}

type fakeContainer struct {
	id      string
	name    string
	labels  map[string]string
	state   string
	created int64
	env     []string
	image   string
}

func newFakeDocker(t *testing.T) *fakeDocker {
	return &fakeDocker{t: t, containers: map[string]*fakeContainer{}}
}

// startOnSocket runs the fake on a unix socket and returns the docker_host
// value that reaches it.
func (f *fakeDocker) startOnSocket() string {
	f.t.Helper()

	// Socket paths are limited to about 100 bytes, so a short directory is
	// used rather than the test's own temp path, which can be long.
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		f.t.Fatalf("creating socket dir: %v", err)
	}
	socket := filepath.Join(dir, "d.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		f.t.Fatalf("listening on %s: %v", socket, err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()

	f.t.Cleanup(func() {
		server.Close()
		_ = os.RemoveAll(dir)
	})
	return "unix://" + socket
}

func (f *fakeDocker) running(tier string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, c := range f.containers {
		if c.labels["autoscaler.tier"] == tier && c.state == "running" {
			count++
		}
	}
	return count
}

func (f *fakeDocker) envOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.name == name {
			return c.env
		}
	}
	return nil
}

func (f *fakeDocker) anyEnv() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		return c.env
	}
	return nil
}

func (f *fakeDocker) sawRequest(substring string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, substring) {
			return true
		}
	}
	return false
}

func (f *fakeDocker) withContainer(name, tier, state string, created int64) *fakeDocker {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	f.containers[id] = &fakeContainer{
		id: id, name: name, state: state, created: created,
		labels: map[string]string{"autoscaler.target": "storhall", "autoscaler.tier": tier},
	}
	return f
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/containers/json") && r.Method == http.MethodGet:
		f.serveList(w, r)
	case strings.HasSuffix(path, "/containers/create") && r.Method == http.MethodPost:
		f.serveCreate(w, r)
	case strings.HasSuffix(path, "/start") && r.Method == http.MethodPost:
		f.serveTransition(w, r, "/start", "running")
	case strings.HasSuffix(path, "/stop") && r.Method == http.MethodPost:
		f.serveTransition(w, r, "/stop", "exited")
	case strings.HasSuffix(path, "/images/create") && r.Method == http.MethodPost:
		f.servePull(w, r)
	case strings.Contains(path, "/containers/") && r.Method == http.MethodDelete:
		f.serveRemove(w, r)
	case strings.HasSuffix(path, "/_ping"):
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, `{"message":"no such endpoint: `+path+`"}`, http.StatusNotFound)
	}
}

func (f *fakeDocker) serveList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	wanted := parseLabelFilters(r.URL.Query().Get("filters"))

	items := []any{}
	for _, c := range f.containers {
		if !hasLabels(c.labels, wanted) {
			continue
		}
		items = append(items, map[string]any{
			"Id":      c.id,
			"Names":   []string{"/" + c.name},
			"State":   c.state,
			"Created": c.created,
			"Labels":  c.labels,
			"Image":   c.image,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (f *fakeDocker) serveCreate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failNextCreate {
		f.failNextCreate = false
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"message": "no such image",
		})
		return
	}

	var body struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "malformed body"})
		return
	}

	name := r.URL.Query().Get("name")
	for _, c := range f.containers {
		if c.name == name {
			writeJSON(w, http.StatusConflict, map[string]any{
				"message": "container name is already in use",
			})
			return
		}
	}

	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	f.containers[id] = &fakeContainer{
		id: id, name: name, labels: body.Labels, state: "created",
		created: int64(f.nextID), env: body.Env, image: body.Image,
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": id})
}

func (f *fakeDocker) serveTransition(w http.ResponseWriter, r *http.Request, suffix, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := containerID(r.URL.Path, suffix)
	c, ok := f.containers[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "no such container"})
		return
	}
	c.state = state
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDocker) serveRemove(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id := parts[len(parts)-1]
	if _, ok := f.containers[id]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "no such container"})
		return
	}
	delete(f.containers, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDocker) servePull(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pulled = append(f.pulled, r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"Downloaded"}`))
}

func containerID(path, suffix string) string {
	trimmed := strings.TrimSuffix(path, suffix)
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	return parts[len(parts)-1]
}

// parseLabelFilters reads Docker's filters query parameter, which is a JSON
// object of filter name to a list of values.
func parseLabelFilters(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		return nil
	}

	wanted := map[string]string{}
	for _, entry := range filters["label"] {
		key, value, found := strings.Cut(entry, "=")
		if found {
			wanted[key] = value
		}
	}
	return wanted
}

func hasLabels(labels, wanted map[string]string) bool {
	for k, v := range wanted {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// names is the containers of one tier, sorted, so a test can say which
// survived a scale-down.
func (f *fakeDocker) names(tier string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string
	for _, c := range f.containers {
		if c.labels["autoscaler.tier"] == tier {
			out = append(out, c.name)
		}
	}
	sort.Strings(out)
	return out
}
