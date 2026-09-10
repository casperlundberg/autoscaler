// Package colonypods runs ColonyOS executors as Kubernetes pods.
//
// It is the composition of the two platforms this service already speaks:
// Kubernetes owns the capacity, and ColonyOS owns the queue that capacity
// serves. That split is what makes this adapter more useful than plain
// Kubernetes — it can see the work, so a target here is polled rather than
// driven — and it is why it provisions rather than merely scales: the pods it
// creates are the executors that join the colony.
package colonypods

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes"
)

// Field names, in one place so the schema, the adapter and the errors agree.
const (
	FieldAPIServer     = "api_server"
	FieldInCluster     = "in_cluster"
	FieldNamespace     = "namespace"
	FieldCACertificate = "ca_certificate"
	FieldInsecure      = "insecure_skip_verify"
	FieldTimeout       = "request_timeout_seconds"

	FieldExecutorImage   = "executor_image"
	FieldLocalDeploy     = "local_deployment"
	FieldCloudDeploy     = "cloud_deployment"
	FieldKeySecretName   = "key_secret_name"
	FieldImagePullSecret = "image_pull_secret"
	FieldServiceAccount  = "service_account_name"
	FieldCPURequest      = "cpu_request"
	FieldMemoryRequest   = "memory_request"
	FieldCPULimit        = "cpu_limit"
	FieldMemoryLimit     = "memory_limit"

	FieldColoniesHost       = "colonies_host"
	FieldColoniesPort       = "colonies_port"
	FieldColoniesTLS        = "colonies_tls"
	FieldColoniesSkipVerify = "colonies_skip_tls_verify"
	FieldColonyName         = "colony_name"
	FieldExecutorType       = "executor_type"
	FieldMaxProcesses       = "max_processes"
	FieldArrivalWindow      = "arrival_window_seconds"
	FieldJobSecondsKey      = "job_seconds_env_key"
	FieldDefaultJobSeconds  = "default_job_seconds"

	CredentialBearerToken = "bearer_token"
	CredentialColoniesKey = "colonies_prvkey"
)

const (
	tierLocal = "local"
	tierCloud = "cloud"

	// secretKeyName is the entry inside the Kubernetes Secret holding the
	// ColonyOS private key.
	secretKeyName = "prvkey"
)

// Adapter provisions ColonyOS executors as Kubernetes Deployments.
type Adapter struct {
	serviceAccountDir string
	env               func(string) string
}

// New returns an adapter reading in-cluster credentials from the standard
// mount path.
func New() *Adapter {
	return &Adapter{
		serviceAccountDir: "/var/run/secrets/kubernetes.io/serviceaccount",
		env:               os.Getenv,
	}
}

// NewWithServiceAccount returns an adapter reading in-cluster credentials from
// a given directory and environment.
func NewWithServiceAccount(dir string, env func(string) string) *Adapter {
	return &Adapter{serviceAccountDir: dir, env: env}
}

// Kind identifies this adapter.
func (a *Adapter) Kind() platform.Kind { return platform.KindColonyOSPods }

// Schema declares everything needed to stand up an executor pool from nothing.
func (a *Adapter) Schema() platform.Schema {
	return platform.Schema{
		Kind: platform.KindColonyOSPods,
		Summary: "Runs ColonyOS executors as Kubernetes pods. Reads the queue from " +
			"the ColonyOS server and creates the executor Deployments itself.",
		SeesWorkload: true,
		Config: []platform.Field{
			{Name: FieldAPIServer, Label: "Kubernetes API server URL",
				Description: "Required unless in_cluster is set.", Example: "https://k8s.example.org:6443"},
			{Name: FieldInCluster, Label: "Use the pod's own service account",
				Description: "Set when the autoscaler runs inside the cluster it provisions into.",
				Default:     "false"},
			{Name: FieldNamespace, Label: "Namespace", Required: true,
				Description: "Where the executor Deployments and their key Secret live.",
				Example:     "mining"},
			{Name: FieldCACertificate, Label: "Kubernetes API CA certificate (PEM)"},
			{Name: FieldInsecure, Label: "Skip Kubernetes TLS verification", Default: "false"},
			{Name: FieldTimeout, Label: "Kubernetes request timeout (seconds)", Default: "10"},

			{Name: FieldExecutorImage, Label: "Executor image", Required: true,
				Description: "The ColonyOS executor image the pods run.",
				Example:     "ghcr.io/example/colonyos-executor:v1"},
			{Name: FieldLocalDeploy, Label: "On-premise Deployment name",
				Description: "Defaults to executor-<target id>-local. Set it to adopt an existing pool."},
			{Name: FieldCloudDeploy, Label: "Cloud Deployment name",
				Description: "Defaults to executor-<target id>-cloud."},
			{Name: FieldKeySecretName, Label: "Key Secret name",
				Description: "The Secret this service writes the executor private key into. Defaults to colonies-<target id>."},
			{Name: FieldImagePullSecret, Label: "Image pull secret",
				Description: "Needed when the executor image is in a private registry."},
			{Name: FieldServiceAccount, Label: "Pod service account"},
			{Name: FieldCPURequest, Label: "CPU request per executor", Example: "500m"},
			{Name: FieldMemoryRequest, Label: "Memory request per executor", Example: "512Mi"},
			{Name: FieldCPULimit, Label: "CPU limit per executor"},
			{Name: FieldMemoryLimit, Label: "Memory limit per executor"},

			{Name: FieldColoniesHost, Label: "ColonyOS server host", Required: true,
				Example: "colonies-server.colonies"},
			{Name: FieldColoniesPort, Label: "ColonyOS server port", Default: "50080"},
			{Name: FieldColoniesTLS, Label: "ColonyOS server uses TLS", Default: "false"},
			{Name: FieldColoniesSkipVerify, Label: "Skip ColonyOS TLS verification", Default: "false"},
			{Name: FieldColonyName, Label: "Colony name", Required: true, Example: "dev"},
			{Name: FieldExecutorType, Label: "Executor type", Required: true,
				Description: "Scopes both the queue read and the executors created. Several workloads commonly share one colony, and this is what keeps them apart.",
				Example:     "bemis-storhall"},
			{Name: FieldMaxProcesses, Label: "Maximum processes read per cycle",
				Description: "Must exceed the deepest queue expected, and must not exceed the server's own COLONIES_MAX_COUNT.",
				Default:     "10000"},
			{Name: FieldArrivalWindow, Label: "Arrival rate window (seconds)",
				Description: "How far back to look when measuring incoming work.", Default: "300"},
			{Name: FieldJobSecondsKey, Label: "Execution time env key",
				Description: "The process env entry declaring how long a job takes.", Default: "exec_seconds"},
			{Name: FieldDefaultJobSeconds, Label: "Default execution time (seconds)",
				Description: "Used for work that does not declare its own.", Default: "30"},
		},
		Credentials: []platform.Field{
			{Name: CredentialBearerToken, Label: "Kubernetes bearer token",
				Description: "Permitted to read and create deployments, secrets and pods in this namespace. Not required when in_cluster is set."},
			{Name: CredentialColoniesKey, Label: "ColonyOS executor private key", Required: true,
				Description: "Hex-encoded. Used to read the queue, and written into the namespace Secret the executor pods read."},
		},
	}
}

// Validate proves both back ends work before the target is accepted.
func (a *Adapter) Validate(ctx context.Context, target platform.Target) error {
	var problems []string
	if err := a.Schema().Check(target); err != nil {
		problems = append(problems, err.Error())
	}

	kube, err := a.kubeClient(target)
	if err != nil {
		problems = append(problems, err.Error())
	}
	colonies, identity, err := a.coloniesClient(target)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	// A pool that does not exist yet is fine — this adapter creates it. What
	// must be proven now is that the credentials reach the API server at all,
	// which a 404 confirms just as well as a 200.
	if _, err := kube.GetDeployment(ctx, a.deploymentName(target, tierLocal)); err != nil {
		var apiErr *kubernetes.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return fmt.Errorf("kubernetes is not usable with these credentials: %w", err)
		}
	}

	if err := colonies.CheckAccess(ctx, a.Schema().ConfigValue(target, FieldColonyName), identity); err != nil {
		return fmt.Errorf("colonies is not usable with this key: %w", err)
	}
	return nil
}

// Observe reads the pods from Kubernetes and the queue from ColonyOS.
func (a *Adapter) Observe(ctx context.Context, target platform.Target) (platform.Observation, error) {
	kube, err := a.kubeClient(target)
	if err != nil {
		return platform.Observation{}, err
	}

	localReady, localPending, err := tierCapacity(ctx, kube, a.deploymentName(target, tierLocal))
	if err != nil {
		return platform.Observation{}, err
	}
	cloudReady, cloudPending, err := tierCapacity(ctx, kube, a.deploymentName(target, tierCloud))
	if err != nil {
		return platform.Observation{}, err
	}

	workload, err := a.observeWorkload(ctx, target)
	if err != nil {
		return platform.Observation{}, err
	}

	return platform.Observation{
		Capacity: domain.Capacity{
			LocalReady: localReady, LocalPending: localPending,
			CloudReady: cloudReady, CloudPending: cloudPending,
		},
		Workload: workload,
	}, nil
}

// Apply writes the executor key, creates whichever pools the plan needs, and
// sizes them.
func (a *Adapter) Apply(ctx context.Context, target platform.Target,
	plan domain.Plan) (platform.ApplyResult, error) {
	if plan.LocalExecutors < 0 || plan.CloudExecutors < 0 {
		return platform.ApplyResult{}, fmt.Errorf("plan has a negative executor count: %+v", plan)
	}

	kube, err := a.kubeClient(target)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	key, ok := target.Credentials.Get(CredentialColoniesKey)
	if !ok || strings.TrimSpace(key) == "" {
		return platform.ApplyResult{}, fmt.Errorf("credentials.%s is required to start executors",
			CredentialColoniesKey)
	}

	// The key goes in first. A pool created before the Secret exists starts
	// pods that cannot join the colony and crash-loop until it appears.
	secretName := a.secretName(target)
	if err := kube.EnsureSecret(ctx, secretName, map[string]string{secretKeyName: key}); err != nil {
		return platform.ApplyResult{}, err
	}

	localChanged, err := a.applyTier(ctx, kube, target, tierLocal, plan.LocalExecutors, secretName, true)
	if err != nil {
		return platform.ApplyResult{}, err
	}
	// The cloud pool is created only when it is wanted. A target whose cloud
	// cap is zero should not accumulate an empty Deployment it never uses.
	cloudChanged, err := a.applyTier(ctx, kube, target, tierCloud, plan.CloudExecutors, secretName,
		plan.CloudExecutors > 0)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	return platform.ApplyResult{
		Applied: plan,
		Changed: localChanged || cloudChanged,
		Detail: fmt.Sprintf("%s=%d, %s=%d",
			a.deploymentName(target, tierLocal), plan.LocalExecutors,
			a.deploymentName(target, tierCloud), plan.CloudExecutors),
	}, nil
}

// applyTier creates the pool if needed and scales it. createIfMissing is false
// for a tier the plan does not want and that does not already exist.
func (a *Adapter) applyTier(ctx context.Context, kube *kubernetes.Client, target platform.Target,
	tier string, replicas int, secretName string, createIfMissing bool) (bool, error) {
	name := a.deploymentName(target, tier)

	existing, err := kube.GetDeployment(ctx, name)
	if err != nil {
		var apiErr *kubernetes.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return false, fmt.Errorf("reading deployment %q: %w", name, err)
		}
		if !createIfMissing {
			return false, nil
		}
		created, err := kube.EnsureDeployment(ctx, a.deploymentSpec(target, tier, replicas, secretName))
		if err != nil {
			return false, err
		}
		return created, nil
	}

	if existing.Replicas == replicas {
		return false, nil
	}
	if err := kube.ScaleDeployment(ctx, name, replicas); err != nil {
		return false, fmt.Errorf("scaling deployment %q from %d to %d: %w",
			name, existing.Replicas, replicas, err)
	}
	return true, nil
}

func (a *Adapter) deploymentSpec(target platform.Target, tier string, replicas int,
	secretName string) kubernetes.DeploymentSpec {
	schema := a.Schema()
	name := a.deploymentName(target, tier)

	var pullSecrets []string
	if s := schema.ConfigValue(target, FieldImagePullSecret); s != "" {
		pullSecrets = []string{s}
	}

	return kubernetes.DeploymentSpec{
		Name:     name,
		Image:    schema.ConfigValue(target, FieldExecutorImage),
		Replicas: replicas,
		Labels: map[string]string{
			"app":    "colonyos-executor",
			"tier":   tier,
			"target": target.ID,
		},
		Env: map[string]string{
			"COLONIES_SERVER_HOST":   schema.ConfigValue(target, FieldColoniesHost),
			"COLONIES_SERVER_PORT":   schema.ConfigValue(target, FieldColoniesPort),
			"COLONIES_SERVER_TLS":    schema.ConfigValue(target, FieldColoniesTLS),
			"COLONIES_COLONY_NAME":   schema.ConfigValue(target, FieldColonyName),
			"COLONIES_EXECUTOR_TYPE": schema.ConfigValue(target, FieldExecutorType),
			"AUTOSCALER_TIER":        tier,
		},
		SecretEnv: map[string]kubernetes.SecretKeyRef{
			"COLONIES_PRVKEY": {Secret: secretName, Key: secretKeyName},
		},
		// ColonyOS executors register by name and must not collide, and the
		// only per-pod unique value known at scheduling time is the pod name.
		FieldEnv: map[string]string{
			"COLONIES_EXECUTOR_NAME": "metadata.name",
		},
		ImagePullSecrets:   pullSecrets,
		CPURequest:         schema.ConfigValue(target, FieldCPURequest),
		MemoryRequest:      schema.ConfigValue(target, FieldMemoryRequest),
		CPULimit:           schema.ConfigValue(target, FieldCPULimit),
		MemoryLimit:        schema.ConfigValue(target, FieldMemoryLimit),
		ServiceAccountName: schema.ConfigValue(target, FieldServiceAccount),
	}
}

func (a *Adapter) observeWorkload(ctx context.Context, target platform.Target) (*platform.Workload, error) {
	schema := a.Schema()

	colonies, identity, err := a.coloniesClient(target)
	if err != nil {
		return nil, err
	}

	colonyName := schema.ConfigValue(target, FieldColonyName)
	executorType := schema.ConfigValue(target, FieldExecutorType)
	maxProcesses, err := schema.ConfigInt(target, FieldMaxProcesses)
	if err != nil {
		return nil, err
	}

	waiting, err := colonies.GetProcesses(ctx, colonyName, colonyos.StateWaiting,
		executorType, maxProcesses, identity)
	if err != nil {
		return nil, fmt.Errorf("reading the waiting queue: %w", err)
	}
	running, err := colonies.GetProcesses(ctx, colonyName, colonyos.StateRunning,
		executorType, maxProcesses, identity)
	if err != nil {
		return nil, fmt.Errorf("reading the running processes: %w", err)
	}

	arrivalWindow, err := schema.ConfigInt(target, FieldArrivalWindow)
	if err != nil {
		return nil, err
	}
	defaultJobSeconds, err := schema.ConfigInt(target, FieldDefaultJobSeconds)
	if err != nil {
		return nil, err
	}

	workload, err := colonyos.BuildWorkload(waiting, running, colonyos.WorkloadOptions{
		Now:               platform.CycleTime(ctx),
		ArrivalWindow:     time.Duration(arrivalWindow) * time.Second,
		JobSecondsEnvKey:  schema.ConfigValue(target, FieldJobSecondsKey),
		DefaultJobSeconds: float64(defaultJobSeconds),
	})
	if err != nil {
		return nil, err
	}
	return &workload, nil
}

// tierCapacity reads one pool, treating a pool that does not exist as empty.
// That is not an error: it is the normal state before the first scale-up, and
// the whole reason this adapter can provision from nothing.
func tierCapacity(ctx context.Context, kube *kubernetes.Client, name string) (ready, pending int, err error) {
	deployment, err := kube.GetDeployment(ctx, name)
	if err != nil {
		var apiErr *kubernetes.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("reading deployment %q: %w", name, err)
	}

	ready, err = kube.CountReadyPods(ctx, deployment.Selector)
	if err != nil {
		return 0, 0, fmt.Errorf("counting pods of deployment %q: %w", name, err)
	}
	pending = deployment.Replicas - ready
	if pending < 0 {
		pending = 0
	}
	return ready, pending, nil
}

func (a *Adapter) kubeClient(target platform.Target) (*kubernetes.Client, error) {
	schema := a.Schema()

	timeoutSeconds, err := schema.ConfigInt(target, FieldTimeout)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(timeoutSeconds) * time.Second

	if schema.ConfigBool(target, FieldInCluster) {
		return kubernetes.NewInClusterClient(a.serviceAccountDir,
			schema.ConfigValue(target, FieldNamespace), a.env, timeout)
	}

	token, _ := target.Credentials.Get(CredentialBearerToken)
	return kubernetes.NewClient(kubernetes.ClientOptions{
		APIServer:          schema.ConfigValue(target, FieldAPIServer),
		Token:              token,
		CACertificate:      schema.ConfigValue(target, FieldCACertificate),
		Namespace:          schema.ConfigValue(target, FieldNamespace),
		InsecureSkipVerify: schema.ConfigBool(target, FieldInsecure),
		Timeout:            timeout,
	})
}

func (a *Adapter) coloniesClient(target platform.Target) (*colonyos.Client, *colonyos.Identity, error) {
	schema := a.Schema()

	port, err := schema.ConfigInt(target, FieldColoniesPort)
	if err != nil {
		return nil, nil, err
	}
	timeoutSeconds, err := schema.ConfigInt(target, FieldTimeout)
	if err != nil {
		return nil, nil, err
	}

	client, err := colonyos.NewClient(colonyos.ServerConfig{
		Host:          schema.ConfigValue(target, FieldColoniesHost),
		Port:          port,
		TLS:           schema.ConfigBool(target, FieldColoniesTLS),
		SkipTLSVerify: schema.ConfigBool(target, FieldColoniesSkipVerify),
		Timeout:       time.Duration(timeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}

	key, _ := target.Credentials.Get(CredentialColoniesKey)
	identity, err := colonyos.NewIdentity(key)
	if err != nil {
		return nil, nil, fmt.Errorf("credentials.%s is not a usable ColonyOS key: %w",
			CredentialColoniesKey, err)
	}
	return client, identity, nil
}

// deploymentName is the configured name, or one derived from the target so a
// operator registering a target does not have to invent one.
func (a *Adapter) deploymentName(target platform.Target, tier string) string {
	field := FieldLocalDeploy
	if tier == tierCloud {
		field = FieldCloudDeploy
	}
	if name := a.Schema().ConfigValue(target, field); name != "" {
		return name
	}
	return fmt.Sprintf("executor-%s-%s", target.ID, tier)
}

func (a *Adapter) secretName(target platform.Target) string {
	if name := a.Schema().ConfigValue(target, FieldKeySecretName); name != "" {
		return name
	}
	return "colonies-" + target.ID
}
