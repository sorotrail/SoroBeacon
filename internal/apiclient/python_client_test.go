package apiclient

import (
	"os"
	"testing"

	pythonclient "github.com/sorotrail/sorobeacon/clients/python"
)

// TestPythonClientSpecDrift verifies from internal/apiclient that the committed
// Python client in clients/python/sorobeacon/_generated.py stays in lockstep
// with internal/api/openapi.json.
func TestPythonClientSpecDrift(t *testing.T) {
	specPath := pythonclient.DefaultSpecPath()
	outPath := pythonclient.DefaultOutputPath()

	generated, err := pythonclient.GenerateFromFile(specPath)
	if err != nil {
		t.Fatalf("GenerateFromFile(%s): %v", specPath, err)
	}

	committed, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read committed Python client %s: %v", outPath, err)
	}

	if string(committed) != string(generated) {
		t.Fatalf("clients/python/sorobeacon/_generated.py is out of sync with internal/api/openapi.json; regenerate with `go generate ./clients/python/...`")
	}
}
