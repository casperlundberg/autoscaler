// Package colonycontainers runs ColonyOS executors as standalone containers on
// a Docker host, with no orchestrator involved.
//
// This is the platform for capacity that has no Kubernetes: an edge machine at
// a mine site, a workstation, a single cloud VM. The trade is real and worth
// stating plainly — Docker has no Secret mechanism outside Swarm, so the
// ColonyOS private key is passed as an environment variable and is visible to
// anyone who can run `docker inspect` on that host.
package colonycontainers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerAPIVersion is pinned so a host upgrade cannot change the shapes this
// client decodes. Docker keeps versioned endpoints available indefinitely.
const dockerAPIVersion = "v1.43"

// dockerError is a non-2xx response carrying Docker's own explanation, which
// is almost always the useful one ("no such image", "name already in use").
type dockerError struct {
	Status  int
	Message string
	Path    string
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker %s returned %d: %s", e.Path, e.Status, e.Message)
}

// container is the part of a Docker container this service cares about.
type container struct {
	ID      string
	Name    string
	State   string
	Created int64
	Labels  map[string]string
}

// running reports whether this container is doing work. Docker's other states
// — created, restarting, paused, exited — are all "not serving the queue".
func (c container) running() bool { return c.State == "running" }

// dockerClient talks to a Docker daemon over a unix socket or over TCP.
type dockerClient struct {
	http *http.Client
	base string
	host string
}

// dockerTLS is the client certificate material for a TCP daemon.
type dockerTLS struct {
	Certificate string
	Key         string
	CA          string
}

// newDockerClient builds a client for a DOCKER_HOST-style address.
func newDockerClient(host string, tlsConfig dockerTLS, timeout time.Duration) (*dockerClient, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("config.%s is required", FieldDockerHost)
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("config.%s is not a usable address: %w", FieldDockerHost, err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	switch parsed.Scheme {
	case "unix":
		// The daemon is on a socket, so the URL host is meaningless; a
		// placeholder keeps net/http happy while the dialer does the work.
		socket := parsed.Path
		return &dockerClient{
			http: &http.Client{
				Timeout: timeout,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, "unix", socket)
					},
				},
			},
			base: "http://docker/" + dockerAPIVersion,
			host: host,
		}, nil

	case "tcp", "http", "https":
		transport := &http.Transport{}
		scheme := "http"
		if parsed.Scheme == "https" || tlsConfig.Certificate != "" {
			scheme = "https"
			config, err := clientTLS(tlsConfig)
			if err != nil {
				return nil, err
			}
			transport.TLSClientConfig = config
		}
		return &dockerClient{
			http: &http.Client{Timeout: timeout, Transport: transport},
			base: fmt.Sprintf("%s://%s/%s", scheme, parsed.Host, dockerAPIVersion),
			host: host,
		}, nil

	default:
		return nil, fmt.Errorf("config.%s scheme %q is not supported; use unix:// or tcp://",
			FieldDockerHost, parsed.Scheme)
	}
}

func clientTLS(material dockerTLS) (*tls.Config, error) {
	config := &tls.Config{}

	if material.Certificate != "" || material.Key != "" {
		pair, err := tls.X509KeyPair([]byte(material.Certificate), []byte(material.Key))
		if err != nil {
			return nil, fmt.Errorf("the Docker client certificate and key do not form a usable pair: %w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}
	if material.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(material.CA)) {
			return nil, fmt.Errorf("the Docker CA certificate is not valid PEM")
		}
		config.RootCAs = pool
	}
	return config, nil
}

// ping proves the daemon is reachable and the credentials work.
func (d *dockerClient) ping(ctx context.Context) error {
	return d.do(ctx, http.MethodGet, "/_ping", nil, nil, nil, nil)
}

// listContainers returns every container matching a label set, running or not.
func (d *dockerClient) listContainers(ctx context.Context, labels map[string]string) ([]container, error) {
	selectors := make([]string, 0, len(labels))
	for key, value := range labels {
		selectors = append(selectors, key+"="+value)
	}
	filters, err := json.Marshal(map[string][]string{"label": selectors})
	if err != nil {
		return nil, err
	}

	query := url.Values{"all": {"1"}, "filters": {string(filters)}}

	var body []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		State   string            `json:"State"`
		Created int64             `json:"Created"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json", query, nil, nil, &body); err != nil {
		return nil, err
	}

	containers := make([]container, 0, len(body))
	for _, c := range body {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		containers = append(containers, container{
			ID: c.ID, Name: name, State: c.State, Created: c.Created, Labels: c.Labels,
		})
	}
	return containers, nil
}

// containerSpec is one executor container.
type containerSpec struct {
	Name          string
	Image         string
	Env           []string
	Labels        map[string]string
	Network       string
	RestartPolicy string
	CPUs          string
	Memory        string
}

// createContainer creates a container and returns its id.
func (d *dockerClient) createContainer(ctx context.Context, spec containerSpec) (string, error) {
	hostConfig := map[string]any{}
	if spec.Network != "" {
		hostConfig["NetworkMode"] = spec.Network
	}
	if spec.RestartPolicy != "" {
		hostConfig["RestartPolicy"] = map[string]any{"Name": spec.RestartPolicy}
	}
	if spec.Memory != "" {
		bytes, err := parseMemory(spec.Memory)
		if err != nil {
			return "", err
		}
		hostConfig["Memory"] = bytes
	}
	if spec.CPUs != "" {
		nano, err := parseCPUs(spec.CPUs)
		if err != nil {
			return "", err
		}
		hostConfig["NanoCpus"] = nano
	}

	body := map[string]any{
		"Image":      spec.Image,
		"Env":        spec.Env,
		"Labels":     spec.Labels,
		"HostConfig": hostConfig,
	}

	var reply struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodPost, "/containers/create",
		url.Values{"name": {spec.Name}}, body, nil, &reply)
	if err != nil {
		return "", err
	}
	return reply.ID, nil
}

func (d *dockerClient) startContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil, nil)
}

func (d *dockerClient) stopContainer(ctx context.Context, id string, grace time.Duration) error {
	query := url.Values{"t": {fmt.Sprintf("%d", int(grace.Seconds()))}}
	return d.do(ctx, http.MethodPost, "/containers/"+id+"/stop", query, nil, nil, nil)
}

func (d *dockerClient) removeContainer(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil, nil)
}

// pullImage fetches an image. A plain Docker host has no image controller
// pulling on its behalf, so without this every create fails on a host that has
// never run this executor before.
func (d *dockerClient) pullImage(ctx context.Context, image, registryAuth string) error {
	name, tag := splitImage(image)
	query := url.Values{"fromImage": {name}, "tag": {tag}}

	headers := map[string]string{}
	if registryAuth != "" {
		headers["X-Registry-Auth"] = registryAuth
	}
	return d.do(ctx, http.MethodPost, "/images/create", query, nil, headers, nil)
}

// splitImage separates a reference into name and tag, leaving a digest
// reference intact as its own "tag".
func splitImage(image string) (name, tag string) {
	if name, digest, found := strings.Cut(image, "@"); found {
		return name, digest
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, "latest"
}

func (d *dockerClient) do(ctx context.Context, method, path string, query url.Values,
	body any, headers map[string]string, out any) error {
	target := d.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request to docker %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("building the request to docker %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling docker at %s: %w", d.host, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("reading the docker %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &dockerError{Status: resp.StatusCode, Message: dockerMessage(payload), Path: path}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decoding the docker %s response: %w", path, err)
	}
	return nil
}

func dockerMessage(payload []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && body.Message != "" {
		return body.Message
	}
	if len(payload) > 400 {
		return string(payload[:400])
	}
	return string(payload)
}
