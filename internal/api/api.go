// Package api is the service's HTTP surface.
//
// It is thin on purpose: parse, delegate, render. Every rule about what is
// allowed lives in the package that owns the thing being changed, so the same
// rules apply whether a change arrives over HTTP or from the control loop.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/casperlundberg/autoscaler/internal/buildinfo"
	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/controller"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

// maxBodyBytes bounds a request body. Settings and target documents are small;
// anything larger is a mistake or an attack.
const maxBodyBytes = 1 << 20

// Options is everything the HTTP layer needs.
type Options struct {
	Platforms  *platform.Registry
	Targets    *registry.Registry
	Controller *controller.Controller

	// Token, when set, is required as a bearer token on every endpoint except
	// the probes. This service holds Kubernetes tokens, ColonyOS keys and
	// Docker certificates, so leaving it unset is only reasonable behind
	// something else that authenticates.
	Token string

	Logger *slog.Logger
}

type server struct {
	Options
}

// route is one endpoint. Keeping them in a table rather than inline lets the
// spec be checked against the implementation, which is the only thing that
// keeps an OpenAPI document honest over time.
type route struct {
	Method  string
	Path    string
	Public  bool
	Handler func(*server) http.HandlerFunc
}

var routes = []route{
	// Probes are unauthenticated. A kubelet cannot carry a credential, and
	// requiring one would have the pod killed for being unauthenticated rather
	// than unhealthy.
	{http.MethodGet, "/healthz", true, func(s *server) http.HandlerFunc { return s.health }},
	{http.MethodGet, "/readyz", true, func(s *server) http.HandlerFunc { return s.health }},

	{http.MethodGet, "/v1/version", false, func(s *server) http.HandlerFunc { return s.version }},
	{http.MethodGet, "/v1/platforms", false, func(s *server) http.HandlerFunc { return s.listPlatforms }},
	{http.MethodGet, "/v1/settings/defaults", false, func(s *server) http.HandlerFunc { return s.defaultSettings }},

	{http.MethodGet, "/v1/targets", false, func(s *server) http.HandlerFunc { return s.listTargets }},
	{http.MethodPost, "/v1/targets", false, func(s *server) http.HandlerFunc { return s.createTarget }},
	{http.MethodGet, "/v1/targets/{id}", false, func(s *server) http.HandlerFunc { return s.getTarget }},
	{http.MethodPut, "/v1/targets/{id}", false, func(s *server) http.HandlerFunc { return s.updateTarget }},
	{http.MethodDelete, "/v1/targets/{id}", false, func(s *server) http.HandlerFunc { return s.deleteTarget }},

	{http.MethodGet, "/v1/targets/{id}/settings", false, func(s *server) http.HandlerFunc { return s.getSettings }},
	{http.MethodPatch, "/v1/targets/{id}/settings", false, func(s *server) http.HandlerFunc { return s.applySettings }},
	{http.MethodPut, "/v1/targets/{id}/settings", false, func(s *server) http.HandlerFunc { return s.applySettings }},
	{http.MethodGet, "/v1/targets/{id}/settings/history", false, func(s *server) http.HandlerFunc { return s.settingsHistory }},

	{http.MethodPost, "/v1/targets/{id}/cycle", false, func(s *server) http.HandlerFunc { return s.runCycle }},
	{http.MethodGet, "/v1/targets/{id}/status", false, func(s *server) http.HandlerFunc { return s.targetStatus }},
}

// Routes is every endpoint this service serves, as method and path. Exported
// so the OpenAPI document can be checked against it.
func Routes() []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// New builds the HTTP handler.
func New(options Options) http.Handler {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	s := &server{Options: options}

	mux := http.NewServeMux()
	for _, r := range routes {
		handler := r.Handler(s)
		if r.Public {
			mux.Handle(r.Method+" "+r.Path, handler)
			continue
		}
		mux.Handle(r.Method+" "+r.Path, s.guarded(handler))
	}
	return mux
}

// guarded wraps a handler in the bearer-token check.
func (s *server) guarded(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && !s.authorised(r) {
			writeError(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		next(w, r)
	})
}

func (s *server) authorised(r *http.Request) bool {
	const prefix = "Bearer "

	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	// Constant time, so a caller cannot learn the token one byte at a time
	// from how long the comparison takes.
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(s.Token)) == 1
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) listPlatforms(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"platforms": s.Platforms.Schemas()})
}

func (s *server) defaultSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, config.DefaultSettings())
}

// targetRequest is the body for creating or replacing a target.
type targetRequest struct {
	Target platform.Target `json:"target"`

	// Settings is optional and is a patch onto the shipped defaults, so a
	// caller only states the numbers it cares about and the rest keep sensible
	// values instead of becoming zero.
	Settings json.RawMessage `json:"settings,omitempty"`
}

func (s *server) createTarget(w http.ResponseWriter, r *http.Request) {
	var request targetRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	settings := config.DefaultSettings()
	if len(request.Settings) > 0 {
		if err := json.Unmarshal(request.Settings, &settings); err != nil {
			writeError(w, http.StatusBadRequest, "settings: "+err.Error())
			return
		}
	}

	snapshot, err := s.Targets.Create(r.Context(), request.Target, settings)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}

func (s *server) listTargets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"targets": s.Targets.List()})
}

func (s *server) getTarget(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.Targets.Get(r.PathValue("id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *server) updateTarget(w http.ResponseWriter, r *http.Request) {
	var request targetRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The path is the authority on which target this is, so a body naming a
	// different one cannot rename or overwrite another target by accident.
	request.Target.ID = r.PathValue("id")

	snapshot, err := s.Targets.Update(r.Context(), request.Target)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *server) deleteTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.Targets.Delete(r.PathValue("id")); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	store, err := s.Targets.Settings(r.PathValue("id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeSettings(w, store.Current())
}

func (s *server) applySettings(w http.ResponseWriter, r *http.Request) {
	patch, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading the settings document: "+err.Error())
		return
	}

	expected, err := expectedVersion(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	snapshot, err := s.Targets.ApplySettings(r.PathValue("id"), patch, expected, actor(r))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeSettings(w, snapshot)
}

func (s *server) settingsHistory(w http.ResponseWriter, r *http.Request) {
	store, err := s.Targets.Settings(r.PathValue("id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": store.History(limit)})
}

// cycleRequest is the body for driving one decision.
type cycleRequest struct {
	At       time.Time          `json:"at"`
	Workload *platform.Workload `json:"workload,omitempty"`
}

func (s *server) runCycle(w http.ResponseWriter, r *http.Request) {
	var request cycleRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.Controller.Cycle(r.Context(), r.PathValue("id"), controller.Input{
		At: request.At, Workload: request.Workload,
	})
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) targetStatus(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.Targets.Get(r.PathValue("id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":               snapshot.Target.ID,
		"kind":             snapshot.Target.Kind,
		"mode":             snapshot.Target.Mode,
		"settings_version": snapshot.Settings.Version,
		"last_decision":    snapshot.LastDecision,
		"last_error":       snapshot.LastError,
		"last_cycle_at":    snapshot.LastCycleAt,
	})
}

func writeSettings(w http.ResponseWriter, snapshot config.Snapshot) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    snapshot.Version,
		"updated_at": snapshot.UpdatedAt,
		"updated_by": snapshot.UpdatedBy,
		"settings":   snapshot.Settings,
		// Returned with the document rather than as a separate call, so an
		// operator sees a questionable setting at the moment they save it.
		"warnings": snapshot.Settings.Warnings(),
	})
}

// expectedVersion reads the optimistic-concurrency guard from the query.
func expectedVersion(r *http.Request) (*int64, error) {
	raw := r.URL.Query().Get("expected_version")
	if raw == "" {
		return nil, nil
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("expected_version=%q is not a version number", raw)
	}
	return &version, nil
}

// actor is who to record against a settings change. There is no user model
// here — this service sits behind something that authenticates people — so the
// caller identifies itself and the value is treated as a label, never as a
// permission.
func actor(r *http.Request) string {
	if who := r.Header.Get("X-Autoscaler-Actor"); who != "" {
		return who
	}
	return "api"
}

func decodeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	// A misspelled field that is silently ignored is worse than a rejected
	// request: the caller is told it succeeded and it did not.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("request body: %w", err)
	}
	return nil
}

// writeRegistryError maps a domain failure onto a status code, so callers can
// branch on the code rather than matching error text.
func writeRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, registry.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, config.ErrVersionConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		// Everything else that reaches here is the caller having asked for
		// something that cannot be done: an unusable target, settings that
		// would not run, a cycle with no workload.
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// version is which code is making the decisions: the release and commit this
// process was built from. A client recording results records this with them,
// so a result can be traced to, and rebuilt from, the code that produced it.
func (s *server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Read())
}
