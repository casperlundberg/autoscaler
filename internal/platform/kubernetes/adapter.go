package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
)

// Config and credential field names, in one place so the schema, the adapter
// and the error messages cannot drift apart.
const (
	FieldAPIServer      = "api_server"
	FieldInCluster      = "in_cluster"
	FieldNamespace      = "namespace"
	FieldLocalDeploy    = "local_deployment"
	FieldCloudDeploy    = "cloud_deployment"
	FieldCACertificate  = "ca_certificate"
	FieldInsecure       = "insecure_skip_verify"
	FieldTimeoutSeconds = "request_timeout_seconds"

	CredentialBearerToken = "bearer_token"
)

// defaultServiceAccountDir is where Kubernetes mounts a pod's own credentials.
const defaultServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Adapter scales two Deployments — one per tier — to match a plan.
//
// This is the plainest platform: it has no idea what the pods do, and no way
// to see the queue they serve. A target on this platform is therefore driven,
// with the workload supplied by whoever calls the autoscaler.
type Adapter struct {
	serviceAccountDir string
	env               func(string) string
}

// New returns an adapter reading in-cluster credentials from the standard
// mount path.
func New() *Adapter {
	return &Adapter{serviceAccountDir: defaultServiceAccountDir, env: os.Getenv}
}

// NewWithServiceAccount returns an adapter that reads in-cluster credentials
// from a given directory and environment, which is how the in-cluster path is
// tested without a cluster.
func NewWithServiceAccount(dir string, env func(string) string) *Adapter {
	return &Adapter{serviceAccountDir: dir, env: env}
}

// Kind identifies this adapter.
func (a *Adapter) Kind() platform.Kind { return platform.KindKubernetes }

// Schema declares what a Kubernetes target needs.
func (a *Adapter) Schema() platform.Schema {
	return platform.Schema{
		Kind: platform.KindKubernetes,
		Summary: "Scales one Deployment per tier. Knows nothing about the jobs the " +
			"pods run, so the workload must be supplied with each decision.",
		SeesWorkload: false,
		Config: []platform.Field{
			{
				Name:        FieldAPIServer,
				Label:       "API server URL",
				Description: "Required unless in_cluster is set.",
				Example:     "https://k8s.example.org:6443",
			},
			{
				Name:        FieldInCluster,
				Label:       "Use the pod's own service account",
				Description: "Set when the autoscaler runs inside the cluster it scales. The API server address, CA and token then come from the mounted service account.",
				Default:     "false",
			},
			{
				Name:        FieldNamespace,
				Label:       "Namespace",
				Description: "The namespace holding the executor Deployments.",
				Required:    true,
				Example:     "mining",
			},
			{
				Name:        FieldLocalDeploy,
				Label:       "On-premise Deployment",
				Description: "The Deployment scaled for the local tier.",
				Required:    true,
				Example:     "executor-storhall-local",
			},
			{
				Name:        FieldCloudDeploy,
				Label:       "Cloud Deployment",
				Description: "The Deployment scaled for the cloud tier. Leave empty for an on-premise-only target.",
				Example:     "executor-storhall-cloud",
			},
			{
				Name:        FieldCACertificate,
				Label:       "API server CA certificate (PEM)",
				Description: "Needed when the API server presents a certificate the host does not already trust.",
			},
			{
				Name:        FieldInsecure,
				Label:       "Skip TLS verification",
				Description: "For development clusters with self-signed certificates. Off unless deliberately set.",
				Default:     "false",
			},
			{
				Name:    FieldTimeoutSeconds,
				Label:   "Request timeout (seconds)",
				Default: "10",
			},
		},
		Credentials: []platform.Field{
			{
				Name:        CredentialBearerToken,
				Label:       "Bearer token",
				Description: "A ServiceAccount token permitted to get deployments and pods, and to patch deployments/scale, in this namespace. Not required when in_cluster is set.",
			},
		},
	}
}

// Validate proves the target actually works: that the settings are complete,
// the credentials authenticate, and both named Deployments exist.
//
// Registration is the moment to discover a misspelled Deployment name or a
// token without the right Role — not at three in the morning, mid-burst, when
// the first scale-up is rejected.
func (a *Adapter) Validate(ctx context.Context, target platform.Target) error {
	var problems []string
	if err := a.Schema().Check(target); err != nil {
		problems = append(problems, err.Error())
	}

	client, err := a.client(target)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	for _, name := range a.deployments(target) {
		if _, err := client.GetDeployment(ctx, name); err != nil {
			return fmt.Errorf("deployment %q is not reachable with these credentials: %w", name, err)
		}
	}
	return nil
}

// Observe reports what each tier is running.
func (a *Adapter) Observe(ctx context.Context, target platform.Target) (platform.Observation, error) {
	client, err := a.client(target)
	if err != nil {
		return platform.Observation{}, err
	}

	localReady, localPending, err := a.tierCapacity(ctx, client, a.localDeployment(target))
	if err != nil {
		return platform.Observation{}, err
	}
	cloudReady, cloudPending, err := a.tierCapacity(ctx, client, a.cloudDeployment(target))
	if err != nil {
		return platform.Observation{}, err
	}

	return platform.Observation{
		Capacity: domain.Capacity{
			LocalReady: localReady, LocalPending: localPending,
			CloudReady: cloudReady, CloudPending: cloudPending,
		},
	}, nil
}

// tierCapacity splits one Deployment into ready and pending.
//
// Pending is the Deployment's own desired count minus what is Ready, not a
// count of not-ready pods. During a scale-up the extra pods do not exist yet,
// and a controller that could not see the request it had already made would
// issue it again on every cycle and end up with several times the capacity it
// intended.
func (a *Adapter) tierCapacity(ctx context.Context, client *Client, name string) (ready, pending int, err error) {
	if name == "" {
		return 0, 0, nil
	}

	deployment, err := client.GetDeployment(ctx, name)
	if err != nil {
		return 0, 0, fmt.Errorf("reading deployment %q: %w", name, err)
	}
	ready, err = client.CountReadyPods(ctx, deployment.Selector)
	if err != nil {
		return 0, 0, fmt.Errorf("counting pods of deployment %q: %w", name, err)
	}

	pending = deployment.Replicas - ready
	if pending < 0 {
		// Normal mid-scale-down: pods that are terminating can still report
		// Ready for a moment after the replica count has already dropped.
		pending = 0
	}
	return ready, pending, nil
}

// Apply scales each tier to the plan, skipping a tier that already matches.
func (a *Adapter) Apply(ctx context.Context, target platform.Target,
	plan domain.Plan) (platform.ApplyResult, error) {
	if plan.LocalExecutors < 0 || plan.CloudExecutors < 0 {
		return platform.ApplyResult{}, fmt.Errorf("plan has a negative executor count: %+v", plan)
	}

	client, err := a.client(target)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	cloudDeployment := a.cloudDeployment(target)
	if plan.CloudExecutors > 0 && cloudDeployment == "" {
		return platform.ApplyResult{}, fmt.Errorf(
			"plan asks for %d cloud executors but config.%s is not set on target %q: "+
				"either name a cloud Deployment or set cloud_executor_cap to 0",
			plan.CloudExecutors, FieldCloudDeploy, target.Name)
	}

	localChanged, err := a.scaleTier(ctx, client, a.localDeployment(target), plan.LocalExecutors)
	if err != nil {
		return platform.ApplyResult{}, err
	}
	cloudChanged, err := a.scaleTier(ctx, client, cloudDeployment, plan.CloudExecutors)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	return platform.ApplyResult{
		Applied: plan,
		Changed: localChanged || cloudChanged,
		Detail: fmt.Sprintf("%s=%d, %s=%d", a.localDeployment(target), plan.LocalExecutors,
			orNone(cloudDeployment), plan.CloudExecutors),
	}, nil
}

// scaleTier writes only when the count actually differs. A no-op PATCH is a
// write to the API server and an entry in the audit log for every cycle of
// every target, which drowns the writes that mean something.
func (a *Adapter) scaleTier(ctx context.Context, client *Client, name string, replicas int) (bool, error) {
	if name == "" {
		return false, nil
	}

	deployment, err := client.GetDeployment(ctx, name)
	if err != nil {
		return false, fmt.Errorf("reading deployment %q before scaling: %w", name, err)
	}
	if deployment.Replicas == replicas {
		return false, nil
	}
	if err := client.ScaleDeployment(ctx, name, replicas); err != nil {
		return false, fmt.Errorf("scaling deployment %q from %d to %d: %w",
			name, deployment.Replicas, replicas, err)
	}
	return true, nil
}

func (a *Adapter) client(target platform.Target) (*Client, error) {
	schema := a.Schema()

	timeoutSeconds, err := schema.ConfigInt(target, FieldTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(timeoutSeconds) * time.Second

	if schema.ConfigBool(target, FieldInCluster) {
		return NewInClusterClient(a.serviceAccountDir,
			schema.ConfigValue(target, FieldNamespace), a.env, timeout)
	}

	token, _ := target.Credentials.Get(CredentialBearerToken)
	return NewClient(ClientOptions{
		APIServer:          schema.ConfigValue(target, FieldAPIServer),
		Token:              token,
		CACertificate:      schema.ConfigValue(target, FieldCACertificate),
		Namespace:          schema.ConfigValue(target, FieldNamespace),
		InsecureSkipVerify: schema.ConfigBool(target, FieldInsecure),
		Timeout:            timeout,
	})
}

func (a *Adapter) localDeployment(target platform.Target) string {
	return a.Schema().ConfigValue(target, FieldLocalDeploy)
}

func (a *Adapter) cloudDeployment(target platform.Target) string {
	return a.Schema().ConfigValue(target, FieldCloudDeploy)
}

func (a *Adapter) deployments(target platform.Target) []string {
	names := []string{a.localDeployment(target)}
	if cloud := a.cloudDeployment(target); cloud != "" {
		names = append(names, cloud)
	}
	return names
}

func orNone(s string) string {
	if s == "" {
		return "(no cloud deployment)"
	}
	return s
}
