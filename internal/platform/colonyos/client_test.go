package colonyos_test

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos/colonytest"
)

var submitted = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func clientFor(t *testing.T, serverURL string) *colonyos.Client {
	t.Helper()

	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parsing server URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parsing port: %v", err)
	}

	client, err := colonyos.NewClient(colonyos.ServerConfig{
		Host: parsed.Hostname(), Port: port, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient() = %v", err)
	}
	return client
}

func identity(t *testing.T) *colonyos.Identity {
	t.Helper()
	id, err := colonyos.NewIdentity(goldenPrivateKey)
	if err != nil {
		t.Fatalf("NewIdentity() = %v", err)
	}
	return id
}

func TestWaitingProcessesAreReadBackWithTheirPriorityAndAge(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	fake.Waiting = []colonyos.Process{
		colonytest.Job("a", 100, submitted, "bemis", map[string]string{"exec_seconds": "12"}),
		colonytest.Job("b", 25, submitted.Add(-time.Minute), "bemis", nil),
	}
	server := fake.Start()

	got, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateWaiting, "bemis", 100, me)
	if err != nil {
		t.Fatalf("GetProcesses() = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("GetProcesses() returned %d processes, want 2", len(got))
	}
	if got[0].Priority != 100 || got[0].ID != "a" {
		t.Errorf("first process = %+v, want id a at priority 100", got[0])
	}
	if got[0].Env["exec_seconds"] != "12" {
		t.Errorf("env = %v, want exec_seconds preserved", got[0].Env)
	}
	if !got[1].SubmittedAt.Equal(submitted.Add(-time.Minute)) {
		t.Errorf("SubmittedAt = %v, want %v", got[1].SubmittedAt, submitted.Add(-time.Minute))
	}
}

// Filtering server-side matters: several mines share one colony, and pulling
// every mine's queue back to filter locally is both slower and vulnerable to
// the server's own result cap truncating the rows that were wanted.
func TestTheExecutorTypeFilterIsSentToTheServer(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	fake.Waiting = []colonyos.Process{
		colonytest.Job("mine-a", 100, submitted, "bemis-storhall", nil),
		colonytest.Job("mine-b", 100, submitted, "bemis-kvarnberg", nil),
	}
	server := fake.Start()

	got, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateWaiting, "bemis-storhall", 100, me)
	if err != nil {
		t.Fatalf("GetProcesses() = %v", err)
	}

	if len(got) != 1 || got[0].ID != "mine-a" {
		t.Errorf("GetProcesses() = %+v, want only the storhall process", got)
	}
	if fake.LastRequest["executortype"] != "bemis-storhall" {
		t.Errorf("executortype sent = %v, want the filter applied server-side",
			fake.LastRequest["executortype"])
	}
	if fake.LastRequest["colonyname"] != "dev" {
		t.Errorf("colonyname sent = %v, want dev", fake.LastRequest["colonyname"])
	}
}

func TestRunningProcessesAreRequestedByState(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	fake.Running = []colonyos.Process{colonytest.Job("r", 100, submitted, "bemis", nil)}
	server := fake.Start()

	got, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateRunning, "bemis", 100, me)
	if err != nil {
		t.Fatalf("GetProcesses() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetProcesses() returned %d, want the one running process", len(got))
	}
}

func TestAnUnknownIdentityIsRejectedByTheServer(t *testing.T) {
	fake := colonytest.New(t, "somebody-else")
	server := fake.Start()

	_, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateWaiting, "bemis", 100, identity(t))
	if err == nil {
		t.Fatal("GetProcesses() = nil error for a non-member")
	}
	// The server's own explanation is the useful one.
	if !strings.Contains(err.Error(), "not a member") {
		t.Errorf("GetProcesses() = %q, want the server's reason preserved", err)
	}
}

func TestAServerErrorIsReportedWithItsMessage(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	fake.FailWith = "database is unavailable"
	server := fake.Start()

	_, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateWaiting, "bemis", 100, me)
	if err == nil || !strings.Contains(err.Error(), "database is unavailable") {
		t.Errorf("GetProcesses() = %v, want the server's message preserved", err)
	}
}

func TestReachingTheResultCapIsReportedRatherThanSilentlyTruncating(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	for i := 0; i < 5; i++ {
		fake.Waiting = append(fake.Waiting, colonytest.Job("p", 100, submitted, "bemis", nil))
	}
	server := fake.Start()

	// A count of 5 returning exactly 5 means the queue was probably longer.
	// Processes come back ordered by priority-time, so a truncated read loses
	// the oldest, lowest-priority work — precisely the work closest to
	// breaching its deadline.
	_, err := clientFor(t, server.URL).GetProcesses(context.Background(),
		"dev", colonyos.StateWaiting, "bemis", 5, me)
	if err == nil {
		t.Fatal("GetProcesses() = nil error when the result hit the cap")
	}
	if !strings.Contains(err.Error(), "cap") && !strings.Contains(err.Error(), "truncat") {
		t.Errorf("GetProcesses() = %q, want it to explain the truncation risk", err)
	}
}

func TestValidateProvesTheKeyIsAMemberOfTheColony(t *testing.T) {
	me := identity(t)
	fake := colonytest.New(t, me.ID())
	server := fake.Start()

	if err := clientFor(t, server.URL).CheckAccess(context.Background(), "dev", me); err != nil {
		t.Errorf("CheckAccess() = %v, want nil", err)
	}
}

func TestValidateFailsForAKeyThatIsNotAMember(t *testing.T) {
	fake := colonytest.New(t, "somebody-else")
	server := fake.Start()

	if err := clientFor(t, server.URL).CheckAccess(context.Background(), "dev", identity(t)); err == nil {
		t.Error("CheckAccess() = nil, want a refusal")
	}
}

func TestAnUnreachableServerIsReportedClearly(t *testing.T) {
	client, err := colonyos.NewClient(colonyos.ServerConfig{
		Host: "127.0.0.1", Port: 1, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient() = %v", err)
	}

	_, err = client.GetProcesses(context.Background(), "dev", colonyos.StateWaiting, "bemis", 10, identity(t))
	if err == nil {
		t.Fatal("GetProcesses() = nil error against a closed port")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("GetProcesses() = %q, want it to name the endpoint it could not reach", err)
	}
}

func TestAMissingHostIsRefusedAtConstruction(t *testing.T) {
	if _, err := colonyos.NewClient(colonyos.ServerConfig{Port: 50080}); err == nil {
		t.Error("NewClient() = nil error with no host")
	}
}
