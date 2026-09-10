package colonyos

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Process states, as ColonyOS numbers them.
const (
	StateWaiting = 0
	StateRunning = 1
)

// Payload types for the two calls this service makes.
const (
	payloadGetProcesses = "getprocessesmsg"
	payloadGetColony    = "getcolonymsg"
)

// Process is the part of a ColonyOS process this service needs: what it is
// waiting for, how urgent it is, and how long it has been waiting.
type Process struct {
	ID           string
	Priority     int
	SubmittedAt  time.Time
	ExecutorType string
	Env          map[string]string
}

// ServerConfig is how to reach a ColonyOS server.
type ServerConfig struct {
	Host          string
	Port          int
	TLS           bool
	SkipTLSVerify bool
	Timeout       time.Duration
}

// Client is a ColonyOS RPC client.
type Client struct {
	http     *http.Client
	endpoint string
	address  string
}

// NewClient builds a client for one ColonyOS server.
func NewClient(cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, fmt.Errorf("colonies server host is required")
	}
	if cfg.Port <= 0 {
		return nil, fmt.Errorf("colonies server port must be > 0, got %d", cfg.Port)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}

	scheme := "http"
	transport := &http.Transport{}
	if cfg.TLS {
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.SkipTLSVerify}
	}

	address := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return &Client{
		http:     &http.Client{Timeout: cfg.Timeout, Transport: transport},
		endpoint: fmt.Sprintf("%s://%s/api", scheme, address),
		address:  address,
	}, nil
}

// GetProcesses reads processes in one state, filtered to one executor type.
//
// The filter is applied by the server. Several mines commonly share a colony,
// and pulling every mine's queue back to sort out locally is both slower and
// exposed to the server's result cap silently dropping rows that were wanted.
func (c *Client) GetProcesses(ctx context.Context, colonyName string, state int,
	executorType string, count int, identity *Identity) ([]Process, error) {
	request := map[string]any{
		"colonyname":   colonyName,
		"count":        count,
		"state":        state,
		"executortype": executorType,
		"label":        "",
		"initiator":    "",
		"msgtype":      payloadGetProcesses,
	}

	payload, err := c.call(ctx, payloadGetProcesses, request, identity)
	if err != nil {
		return nil, err
	}

	var wire []struct {
		ID             string    `json:"processid"`
		SubmissionTime time.Time `json:"submissiontime"`
		Spec           struct {
			Priority   int               `json:"priority"`
			Env        map[string]string `json:"env"`
			Conditions struct {
				ExecutorType string `json:"executortype"`
			} `json:"conditions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, fmt.Errorf("decoding the process list from %s: %w", c.address, err)
	}

	processes := make([]Process, 0, len(wire))
	for _, p := range wire {
		processes = append(processes, Process{
			ID:           p.ID,
			Priority:     p.Spec.Priority,
			SubmittedAt:  p.SubmissionTime,
			ExecutorType: p.Spec.Conditions.ExecutorType,
			Env:          p.Spec.Env,
		})
	}

	if count > 0 && len(processes) >= count {
		// The server truncates at the requested count rather than erroring,
		// and orders results by priority-time — so a truncated read loses the
		// oldest, lowest-priority work first, which is exactly the work
		// closest to breaching its deadline. Scaling on a queue silently
		// missing its most urgent tail would be worse than not scaling at all.
		return processes, fmt.Errorf(
			"colonies returned %d processes, the full requested count: the queue is "+
				"probably longer and has been truncated. Raise this target's "+
				"max_processes (and the server's COLONIES_MAX_COUNT) — results are "+
				"ordered by priority-time, so a truncated read drops the work nearest "+
				"its deadline", len(processes))
	}
	return processes, nil
}

// CheckAccess proves a private key is a member of a colony, which is what
// makes a registration failure visible at registration time.
func (c *Client) CheckAccess(ctx context.Context, colonyName string, identity *Identity) error {
	request := map[string]any{"colonyname": colonyName, "msgtype": payloadGetColony}
	_, err := c.call(ctx, payloadGetColony, request, identity)
	return err
}

// call signs and sends one RPC, returning the decoded reply payload.
func (c *Client) call(ctx context.Context, payloadType string, request any,
	identity *Identity) ([]byte, error) {
	if identity == nil {
		return nil, fmt.Errorf("a colonies private key is required to call %s", payloadType)
	}

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", payloadType, err)
	}
	payload := base64.StdEncoding.EncodeToString(body)

	signature, err := identity.Sign(payload)
	if err != nil {
		return nil, fmt.Errorf("signing %s: %w", payloadType, err)
	}

	envelope, err := json.Marshal(map[string]string{
		"signature":   signature,
		"payloadtype": payloadType,
		"payload":     payload,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the %s envelope: %w", payloadType, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(envelope))
	if err != nil {
		return nil, fmt.Errorf("building the %s request: %w", payloadType, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling colonies at %s: %w", c.address, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the %s reply from %s: %w", payloadType, c.address, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("colonies at %s returned %d for %s: %s",
			c.address, resp.StatusCode, payloadType, truncate(raw))
	}

	var reply struct {
		PayloadType string `json:"payloadtype"`
		Payload     string `json:"payload"`
		Error       bool   `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decoding the %s reply from %s: %w", payloadType, c.address, err)
	}

	decoded, err := base64.StdEncoding.DecodeString(reply.Payload)
	if err != nil {
		return nil, fmt.Errorf("the %s reply payload from %s is not base64: %w",
			payloadType, c.address, err)
	}
	if reply.Error {
		return nil, fmt.Errorf("colonies at %s refused %s: %s",
			c.address, payloadType, failureMessage(decoded))
	}
	return decoded, nil
}

// failureMessage pulls the server's own explanation out of a failure payload.
func failureMessage(payload []byte) string {
	var failure struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &failure); err == nil && failure.Message != "" {
		return failure.Message
	}
	return truncate(payload)
}

func truncate(payload []byte) string {
	if len(payload) > 400 {
		return string(payload[:400])
	}
	return string(payload)
}
