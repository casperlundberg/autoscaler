package api_test

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/casperlundberg/autoscaler/internal/api"
)

// An OpenAPI document is only worth having if it is true. Nothing else in a
// build catches a spec that has drifted from the routes, so this does: the
// document and the route table have to describe exactly the same surface.
func TestTheOpenAPIDocumentDescribesExactlyTheRoutesServed(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("reading the OpenAPI document: %v", err)
	}

	var document struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parsing the OpenAPI document: %v", err)
	}

	documented := map[string]bool{}
	for path, operations := range document.Paths {
		for method := range operations {
			if method == "parameters" {
				continue
			}
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}

	served := map[string]bool{}
	for _, route := range api.Routes() {
		served[route] = true
	}

	for route := range served {
		if !documented[route] {
			t.Errorf("%s is served but not documented in api/openapi.yaml", route)
		}
	}
	for route := range documented {
		if !served[route] {
			t.Errorf("%s is documented in api/openapi.yaml but not served", route)
		}
	}

	if t.Failed() {
		t.Logf("served: %v", sorted(served))
		t.Logf("documented: %v", sorted(documented))
	}
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
