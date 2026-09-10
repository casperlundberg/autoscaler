// Package colonytest is a stand-in for a ColonyOS server that authenticates
// for real: it recovers the caller's identity from the request signature
// exactly as ColonyOS does. A test that passed against a server which ignored
// signatures would prove nothing about talking to one that does not.
//
// It lives in its own package because both ColonyOS adapters — executors as
// pods and executors as containers — read their queue the same way and both
// deserve testing against it.
package colonytest

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
)

// Server speaks the real RPC envelope and, importantly, authenticates
// for real: it recovers the caller's identity from the signature exactly as a
// ColonyOS server does. A test that passed against a server which ignored
// signatures would prove nothing about whether this client can talk to one
// that does not.
type Server struct {
	t *testing.T

	ColonyName string
	members    map[string]bool

	Waiting []colonyos.Process
	Running []colonyos.Process

	// LastRequest records the decoded payload of the most recent call, so a
	// test can assert on what was actually asked for.
	LastRequest map[string]any

	FailWith string
}

func New(t *testing.T, memberID string) *Server {
	return &Server{
		t:          t,
		ColonyName: "dev",
		members:    map[string]bool{memberID: true},
	}
}

// Start runs the server and registers its shutdown with the test.
func (f *Server) Start() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(f.Serve))
	f.t.Cleanup(server.Close)
	return server
}

// Serve handles one RPC request.
func (f *Server) Serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var envelope struct {
		Signature   string `json:"signature"`
		PayloadType string `json:"payloadtype"`
		Payload     string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
		http.Error(w, "malformed envelope", http.StatusBadRequest)
		return
	}

	caller, err := colonyos.RecoverID(envelope.Payload, envelope.Signature)
	if err != nil || !f.members[caller] {
		f.reply(w, true, `{"status":403,"message":"caller is not a member of this colony"}`)
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		http.Error(w, "payload is not base64", http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(decoded, &request); err != nil {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return
	}
	f.LastRequest = request

	if f.FailWith != "" {
		f.reply(w, true, `{"status":500,"message":"`+f.FailWith+`"}`)
		return
	}

	switch envelope.PayloadType {
	case "getprocessesmsg":
		f.replyProcesses(w, request)
	case "getcolonymsg":
		f.reply(w, false, `{"name":"`+f.ColonyName+`"}`)
	default:
		f.reply(w, true, `{"status":400,"message":"unknown payload type"}`)
	}
}

func (f *Server) replyProcesses(w http.ResponseWriter, request map[string]any) {
	source := f.Waiting
	if state, _ := request["state"].(float64); int(state) == colonyos.StateRunning {
		source = f.Running
	}

	wanted, _ := request["executortype"].(string)
	var matching []colonyos.Process
	for _, p := range source {
		if wanted == "" || p.ExecutorType == wanted {
			matching = append(matching, p)
		}
	}

	encoded, err := json.Marshal(toWire(matching))
	if err != nil {
		f.t.Fatalf("encoding processes: %v", err)
	}
	f.reply(w, false, string(encoded))
}

func (f *Server) reply(w http.ResponseWriter, isError bool, payload string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"payloadtype": "reply",
		"payload":     base64.StdEncoding.EncodeToString([]byte(payload)),
		"error":       isError,
	})
}

// toWire renders processes in ColonyOS's own JSON shape, so the client's
// decoding is exercised against the real field names and nesting.
func toWire(processes []colonyos.Process) []map[string]any {
	out := make([]map[string]any, 0, len(processes))
	for _, p := range processes {
		out = append(out, map[string]any{
			"processid":      p.ID,
			"state":          0,
			"submissiontime": p.SubmittedAt.Format(time.RFC3339Nano),
			"spec": map[string]any{
				"priority":   p.Priority,
				"env":        p.Env,
				"conditions": map[string]any{"executortype": p.ExecutorType},
			},
		})
	}
	return out
}

// Job builds a process for a fixture.
func Job(id string, priority int, submittedAt time.Time, executorType string,
	env map[string]string) colonyos.Process {
	return colonyos.Process{
		ID: id, Priority: priority, SubmittedAt: submittedAt,
		ExecutorType: executorType, Env: env,
	}
}
