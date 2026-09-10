package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// SecretKeyRef points an environment variable at an entry in a Secret.
type SecretKeyRef struct {
	Secret string
	Key    string
}

// DeploymentSpec is the executor pool this service knows how to create.
//
// It is deliberately a narrow shape rather than a full Deployment: the
// autoscaler creates one kind of workload — a pool of identical executors —
// and every field here exists because an executor cannot start without it.
// Anything more elaborate belongs in a chart the operator owns, with this
// service scaling what it finds.
type DeploymentSpec struct {
	Name     string
	Image    string
	Replicas int

	// Labels go on the Deployment, its selector and its pods, so the pool can
	// be found again by anything that needs to.
	Labels map[string]string

	// Env is plain configuration. SecretEnv is for anything that must not be
	// readable in a pod spec. FieldEnv maps an environment variable to a pod
	// field path, for values that are only knowable once a pod is scheduled —
	// its own name, above all, since ColonyOS executors register by name and
	// must not collide.
	Env       map[string]string
	SecretEnv map[string]SecretKeyRef
	FieldEnv  map[string]string

	ImagePullSecrets []string

	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string

	ServiceAccountName string
}

// EnsureDeployment creates an executor pool if it is not already there, and
// reports whether it had to.
//
// This is what makes provisioning autonomous rather than merely elastic: given
// credentials and an image, the service can bring a pool into existence, not
// only resize one somebody else built.
//
// An existing Deployment is left completely alone. Re-creating or patching it
// would reset the replica count and roll every running pod, in the middle of
// whatever burst the pool is there to serve.
func (c *Client) EnsureDeployment(ctx context.Context, spec DeploymentSpec) (created bool, err error) {
	if err := spec.validate(); err != nil {
		return false, err
	}

	_, err = c.GetDeployment(ctx, spec.Name)
	if err == nil {
		return false, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return false, fmt.Errorf("checking whether deployment %q exists: %w", spec.Name, err)
	}

	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", c.Namespace)
	if err := c.do(ctx, http.MethodPost, path, nil, spec.manifest(c.Namespace), nil); err != nil {
		return false, fmt.Errorf("creating deployment %q: %w", spec.Name, err)
	}
	return true, nil
}

// EnsureSecret writes key material the executor pods read at startup, creating
// the Secret or replacing it if it is already there.
//
// Replacing matters: a rotated ColonyOS key has to reach the pods, and a
// create that fails on conflict would leave them authenticating with the old
// one until somebody noticed.
func (c *Client) EnsureSecret(ctx context.Context, name string, data map[string]string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a secret name is required")
	}

	encoded := make(map[string]string, len(data))
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata":   map[string]any{"name": name, "namespace": c.Namespace},
		"data":       encoded,
	}

	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/namespaces/%s/secrets", c.Namespace), nil, body, nil)
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		return c.do(ctx, http.MethodPut,
			fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", c.Namespace, name), nil, body, nil)
	}
	return fmt.Errorf("writing secret %q: %w", name, err)
}

func (s DeploymentSpec) validate() error {
	var problems []string
	if strings.TrimSpace(s.Name) == "" {
		problems = append(problems, "a deployment name is required")
	}
	if strings.TrimSpace(s.Image) == "" {
		problems = append(problems, "an executor image is required")
	}
	if s.Replicas < 0 {
		problems = append(problems, fmt.Sprintf("replicas must be >= 0, got %d", s.Replicas))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// manifest renders the Deployment. Written as plain maps rather than typed
// structs because this is the only manifest the service produces, and a
// handful of fields does not justify importing the Kubernetes API types.
func (s DeploymentSpec) manifest(namespace string) map[string]any {
	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	labels["app.kubernetes.io/managed-by"] = "autoscaler"

	container := map[string]any{
		"name":  "executor",
		"image": s.Image,
		"env":   s.envEntries(),
	}
	if resources := s.resources(); len(resources) > 0 {
		container["resources"] = resources
	}

	podSpec := map[string]any{"containers": []any{container}}
	if len(s.ImagePullSecrets) > 0 {
		refs := make([]any, 0, len(s.ImagePullSecrets))
		for _, name := range s.ImagePullSecrets {
			refs = append(refs, map[string]any{"name": name})
		}
		podSpec["imagePullSecrets"] = refs
	}
	if s.ServiceAccountName != "" {
		podSpec["serviceAccountName"] = s.ServiceAccountName
	}

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": s.Name, "namespace": namespace, "labels": labels},
		"spec": map[string]any{
			"replicas": s.Replicas,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
		},
	}
}

// envEntries renders environment in a stable order, so two identical specs
// produce byte-identical manifests and a diff shows only real changes.
func (s DeploymentSpec) envEntries() []any {
	names := make([]string, 0, len(s.Env)+len(s.SecretEnv)+len(s.FieldEnv))
	for name := range s.Env {
		names = append(names, name)
	}
	for name := range s.SecretEnv {
		names = append(names, name)
	}
	for name := range s.FieldEnv {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]any, 0, len(names))
	for _, name := range names {
		if ref, ok := s.SecretEnv[name]; ok {
			// Referenced, never inlined: a private key written as a literal
			// env var is readable by anyone who can read a pod spec, which is
			// a much larger group than those who can read Secrets.
			entries = append(entries, map[string]any{
				"name": name,
				"valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": ref.Secret, "key": ref.Key},
				},
			})
			continue
		}
		if path, ok := s.FieldEnv[name]; ok {
			entries = append(entries, map[string]any{
				"name": name,
				"valueFrom": map[string]any{
					"fieldRef": map[string]any{"fieldPath": path},
				},
			})
			continue
		}
		entries = append(entries, map[string]any{"name": name, "value": s.Env[name]})
	}
	return entries
}

func (s DeploymentSpec) resources() map[string]any {
	requests := map[string]any{}
	if s.CPURequest != "" {
		requests["cpu"] = s.CPURequest
	}
	if s.MemoryRequest != "" {
		requests["memory"] = s.MemoryRequest
	}

	limits := map[string]any{}
	if s.CPULimit != "" {
		limits["cpu"] = s.CPULimit
	}
	if s.MemoryLimit != "" {
		limits["memory"] = s.MemoryLimit
	}

	resources := map[string]any{}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	return resources
}
