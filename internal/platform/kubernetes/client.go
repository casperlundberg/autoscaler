// Package kubernetes provisions capacity by scaling Kubernetes Deployments.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The API server is reached over plain HTTP(S), with no client-go dependency.
// Four operations are needed — read a Deployment, list its pods, patch its
// scale subresource, and create a Deployment for the ColonyOS adapter that
// builds on this one — and client-go would add tens of megabytes and a
// transitive dependency tree for them.

// APIError is a non-2xx response, carrying the API server's own explanation.
// That message is almost always the useful one: "forbidden", "not found",
// which admission webhook rejected the write.
type APIError struct {
	Status  int
	Message string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes %s returned %d: %s", e.Path, e.Status, e.Message)
}

// Client is a minimal Kubernetes API client scoped to one namespace.
type Client struct {
	http      *http.Client
	baseURL   string
	token     string
	Namespace string
}

// ClientOptions is everything needed to reach an API server.
type ClientOptions struct {
	APIServer          string
	Token              string
	CACertificate      string
	Namespace          string
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// NewClient builds a client for an API server reached over the network.
func NewClient(opts ClientOptions) (*Client, error) {
	// Every missing piece at once: an operator registering a target should
	// not have to discover them one rejected request at a time.
	var missing []string
	if strings.TrimSpace(opts.APIServer) == "" {
		missing = append(missing, "config.api_server is required unless config.in_cluster is true")
	}
	if strings.TrimSpace(opts.Token) == "" {
		missing = append(missing, "credentials.bearer_token is required unless config.in_cluster is true")
	}
	if strings.TrimSpace(opts.Namespace) == "" {
		missing = append(missing, "config.namespace is required")
	}
	if len(missing) > 0 {
		return nil, errors.New(strings.Join(missing, "; "))
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{}}
	if opts.CACertificate != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(opts.CACertificate)) {
			return nil, fmt.Errorf("config.ca_certificate is not valid PEM")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	if opts.InsecureSkipVerify {
		// Deliberate and per-target: a development cluster with a self-signed
		// certificate is a real case, and the alternative is operators
		// disabling verification globally instead.
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}

	return &Client{
		http:      &http.Client{Timeout: opts.Timeout, Transport: transport},
		baseURL:   strings.TrimSuffix(opts.APIServer, "/"),
		token:     opts.Token,
		Namespace: opts.Namespace,
	}, nil
}

// Service account paths, as mounted into every pod.
const (
	saTokenFile     = "token"
	saCAFile        = "ca.crt"
	saNamespaceFile = "namespace"
)

// NewInClusterClient builds a client from the service account mounted into
// this pod — the case where the autoscaler runs inside the cluster it scales,
// and the operator has given it a Role rather than a token to store.
//
// It fails loudly on anything missing rather than falling back to an
// unauthenticated client, which would fail later and far less legibly.
func NewInClusterClient(root, namespace string, env func(string) string,
	timeout time.Duration) (*Client, error) {
	token, err := os.ReadFile(filepath.Join(root, saTokenFile))
	if err != nil {
		return nil, fmt.Errorf("config.in_cluster is set but no service account token is "+
			"mounted at %s: %w", filepath.Join(root, saTokenFile), err)
	}
	caPEM, err := os.ReadFile(filepath.Join(root, saCAFile))
	if err != nil {
		return nil, fmt.Errorf("reading the API server CA at %s: %w", filepath.Join(root, saCAFile), err)
	}
	if strings.TrimSpace(namespace) == "" {
		mounted, err := os.ReadFile(filepath.Join(root, saNamespaceFile))
		if err != nil {
			return nil, fmt.Errorf("config.namespace is unset and %s is unreadable: %w",
				filepath.Join(root, saNamespaceFile), err)
		}
		namespace = strings.TrimSpace(string(mounted))
	}

	host, port := env("KUBERNETES_SERVICE_HOST"), env("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("config.in_cluster is set but KUBERNETES_SERVICE_HOST/PORT are not")
	}

	return NewClient(ClientOptions{
		APIServer:     fmt.Sprintf("https://%s:%s", host, port),
		Token:         strings.TrimSpace(string(token)),
		CACertificate: string(caPEM),
		Namespace:     namespace,
		Timeout:       timeout,
	})
}

// Deployment is the part of a Deployment this service cares about.
type Deployment struct {
	Name     string
	Replicas int
	Selector map[string]string
}

// GetDeployment reads a Deployment's desired replica count and pod selector.
func (c *Client) GetDeployment(ctx context.Context, name string) (Deployment, error) {
	var body struct {
		Spec struct {
			Replicas *int `json:"replicas"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
		} `json:"spec"`
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", c.Namespace, name)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &body); err != nil {
		return Deployment{}, err
	}
	if body.Spec.Replicas == nil {
		// A real Deployment always has one. Reading a missing field as zero
		// would feed "no capacity" straight into the next decision.
		return Deployment{}, fmt.Errorf("deployment %q has no .spec.replicas; its desired "+
			"capacity cannot be read", name)
	}
	return Deployment{
		Name:     name,
		Replicas: *body.Spec.Replicas,
		Selector: body.Spec.Selector.MatchLabels,
	}, nil
}

// CountReadyPods counts pods matching a selector whose Ready condition is
// true.
//
// Ready, not merely existing: a pod still pulling its image, or still inside
// its own startup, is not capacity anything can be scheduled onto. Terminating
// pods are excluded for the same reason — they are already gone as far as
// usable throughput goes, even while the API still lists them.
func (c *Client) CountReadyPods(ctx context.Context, selector map[string]string) (int, error) {
	if len(selector) == 0 {
		return 0, fmt.Errorf("cannot count pods without a label selector")
	}

	var body struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}

	query := url.Values{"labelSelector": {formatSelector(selector)}}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods", c.Namespace)
	if err := c.do(ctx, http.MethodGet, path, query, nil, &body); err != nil {
		return 0, err
	}

	ready := 0
	for _, pod := range body.Items {
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready++
				break
			}
		}
	}
	return ready, nil
}

// ScaleDeployment sets a Deployment's replica count through the scale
// subresource.
//
// The subresource, not the Deployment itself: it touches replicas and nothing
// else. A merge patch against the Deployment risks writing the pod template,
// which rolls every pod in the pool — in the middle of the burst that prompted
// the scale.
func (c *Client) ScaleDeployment(ctx context.Context, name string, replicas int) error {
	if replicas < 0 {
		return fmt.Errorf("replicas must be >= 0, got %d", replicas)
	}
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", c.Namespace, name)
	return c.do(ctx, http.MethodPatch, path, nil, patch, nil)
}

func formatSelector(selector map[string]string) string {
	pairs := make([]string, 0, len(selector))
	for k, v := range selector {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values,
	body any, out any) error {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request to %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("building request to %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling kubernetes %s: %w", path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading kubernetes %s response: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: statusMessage(payload), Path: path}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decoding kubernetes %s response: %w", path, err)
	}
	return nil
}

// statusMessage pulls the human-readable reason out of a Status object,
// falling back to the raw body when the response is not one.
func statusMessage(payload []byte) string {
	var status struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &status); err == nil && status.Message != "" {
		return status.Message
	}
	if len(payload) > 400 {
		return string(payload[:400])
	}
	return string(payload)
}
