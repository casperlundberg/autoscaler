package platform_test

import (
	"context"
	"strings"
	"testing"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

// stubProvisioner exists so the registry can be tested without dragging a real
// platform in. Adapters get their own tests against real protocol servers.
type stubProvisioner struct {
	kind   platform.Kind
	schema platform.Schema
}

func (s stubProvisioner) Kind() platform.Kind                             { return s.kind }
func (s stubProvisioner) Schema() platform.Schema                         { return s.schema }
func (s stubProvisioner) Validate(context.Context, platform.Target) error { return nil }
func (s stubProvisioner) Observe(context.Context, platform.Target) (platform.Observation, error) {
	return platform.Observation{}, nil
}
func (s stubProvisioner) Apply(context.Context, platform.Target, domain.Plan) (platform.ApplyResult, error) {
	return platform.ApplyResult{}, nil
}

func stub(kind platform.Kind) stubProvisioner {
	return stubProvisioner{kind: kind, schema: platform.Schema{Kind: kind, Summary: string(kind)}}
}

func TestRegistryFindsARegisteredAdapter(t *testing.T) {
	registry, err := platform.NewRegistry(stub(platform.KindSimulation), stub(platform.KindKubernetes))
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}

	got, err := registry.Get(platform.KindKubernetes)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got.Kind() != platform.KindKubernetes {
		t.Errorf("Get() returned %q", got.Kind())
	}
}

func TestRegistryRefusesTwoAdaptersForOneKind(t *testing.T) {
	_, err := platform.NewRegistry(stub(platform.KindKubernetes), stub(platform.KindKubernetes))
	if err == nil {
		t.Fatal("NewRegistry() = nil error for a duplicate kind, want a refusal")
	}
}

func TestAnUnknownKindSaysWhichKindsExist(t *testing.T) {
	registry, err := platform.NewRegistry(stub(platform.KindSimulation), stub(platform.KindKubernetes))
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}

	_, err = registry.Get("openstack")
	if err == nil {
		t.Fatal("Get(openstack) = nil error, want a refusal")
	}
	// The operator has just mistyped a platform name in an API call; the reply
	// should tell them what they could have typed.
	if !strings.Contains(err.Error(), "kubernetes") || !strings.Contains(err.Error(), "simulation") {
		t.Errorf("Get(openstack) = %q, want it to list the registered kinds", err)
	}
}

func TestKindsAreListedInAStableOrder(t *testing.T) {
	registry, err := platform.NewRegistry(
		stub(platform.KindKubernetes), stub(platform.KindSimulation), stub(platform.KindColonyOSPods))
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}

	first := registry.Kinds()
	for i := 0; i < 10; i++ {
		got := registry.Kinds()
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("Kinds() = %v, want the stable %v", got, first)
			}
		}
	}
	if len(first) != 3 {
		t.Errorf("Kinds() = %v, want 3 entries", first)
	}
}

func schemaWithRequirements() platform.Schema {
	return platform.Schema{
		Kind:    platform.KindKubernetes,
		Summary: "scales two Deployments",
		Config: []platform.Field{
			{Name: "namespace", Required: true},
			{Name: "local_deployment", Required: true},
			{Name: "api_server", Required: false},
		},
		Credentials: []platform.Field{
			{Name: "bearer_token", Required: true},
			{Name: "registry_auth", Required: false},
		},
	}
}

func TestCheckAcceptsATargetWithEverythingRequired(t *testing.T) {
	target := platform.Target{
		Kind:        platform.KindKubernetes,
		Config:      map[string]string{"namespace": "mining", "local_deployment": "executor-local"},
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "t"}),
	}

	if err := schemaWithRequirements().Check(target); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
}

func TestCheckNamesTheMissingConfigField(t *testing.T) {
	target := platform.Target{
		Kind:        platform.KindKubernetes,
		Config:      map[string]string{"namespace": "mining"},
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "t"}),
	}

	err := schemaWithRequirements().Check(target)
	if err == nil {
		t.Fatal("Check() = nil, want a complaint about local_deployment")
	}
	if !strings.Contains(err.Error(), "local_deployment") {
		t.Errorf("Check() = %q, want it to name the missing field", err)
	}
}

func TestCheckNamesTheMissingCredential(t *testing.T) {
	target := platform.Target{
		Kind:   platform.KindKubernetes,
		Config: map[string]string{"namespace": "mining", "local_deployment": "executor-local"},
	}

	err := schemaWithRequirements().Check(target)
	if err == nil {
		t.Fatal("Check() = nil, want a complaint about bearer_token")
	}
	if !strings.Contains(err.Error(), "bearer_token") {
		t.Errorf("Check() = %q, want it to name the missing credential", err)
	}
}

// A registration that is missing three things should say all three, not make
// the operator discover them one failed request at a time.
func TestCheckReportsEveryProblemAtOnce(t *testing.T) {
	err := schemaWithRequirements().Check(platform.Target{Kind: platform.KindKubernetes})
	if err == nil {
		t.Fatal("Check() = nil, want complaints")
	}
	for _, want := range []string{"namespace", "local_deployment", "bearer_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check() = %q, want it to mention %q", err, want)
		}
	}
}

func TestCheckTreatsABlankValueAsMissing(t *testing.T) {
	target := platform.Target{
		Kind:        platform.KindKubernetes,
		Config:      map[string]string{"namespace": "   ", "local_deployment": "executor-local"},
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "t"}),
	}

	if err := schemaWithRequirements().Check(target); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Errorf("Check() = %v, want whitespace treated as missing", err)
	}
}

func TestCheckRejectsConfigKeysTheSchemaDoesNotDefine(t *testing.T) {
	target := platform.Target{
		Kind: platform.KindKubernetes,
		Config: map[string]string{
			"namespace": "mining", "local_deployment": "executor-local", "namesapce": "typo",
		},
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "t"}),
	}

	err := schemaWithRequirements().Check(target)
	if err == nil || !strings.Contains(err.Error(), "namesapce") {
		t.Errorf("Check() = %v, want the unrecognised key named: a silently ignored "+
			"typo is a target that looks configured and is not", err)
	}
}

func TestConfigValueFallsBackToTheSchemaDefault(t *testing.T) {
	schema := platform.Schema{
		Config: []platform.Field{{Name: "port", Default: "50080"}},
	}
	target := platform.Target{Kind: platform.KindColonyOSPods}

	if got := schema.ConfigValue(target, "port"); got != "50080" {
		t.Errorf("ConfigValue(port) = %q, want the default \"50080\"", got)
	}

	target.Config = map[string]string{"port": "9999"}
	if got := schema.ConfigValue(target, "port"); got != "9999" {
		t.Errorf("ConfigValue(port) = %q, want the target's own \"9999\"", got)
	}
}

func TestConfigIntReportsAValueThatIsNotANumber(t *testing.T) {
	schema := platform.Schema{Config: []platform.Field{{Name: "port", Default: "50080"}}}
	target := platform.Target{Config: map[string]string{"port": "not-a-port"}}

	if _, err := schema.ConfigInt(target, "port"); err == nil ||
		!strings.Contains(err.Error(), "port") {
		t.Errorf("ConfigInt(port) = %v, want an error naming the field", err)
	}
}

func TestConfigBoolAcceptsTheUsualSpellings(t *testing.T) {
	schema := platform.Schema{Config: []platform.Field{{Name: "tls", Default: "false"}}}

	for _, tt := range []struct {
		value string
		want  bool
	}{{"true", true}, {"TRUE", true}, {"1", true}, {"yes", true}, {"false", false}, {"0", false}, {"", false}} {
		target := platform.Target{Config: map[string]string{"tls": tt.value}}
		if got := schema.ConfigBool(target, "tls"); got != tt.want {
			t.Errorf("ConfigBool(tls=%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}
