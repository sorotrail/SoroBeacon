// Package sorobeacon_test covers the Helm chart's rendered output.
//
// CI lints the chart and renders it (the `helm` job in
// .github/workflows/ci.yml), but nothing asserted what the rendered manifests
// actually contain — a template edit could quietly drop a probe, leak the
// connection string into the pod spec, or leave a new environment variable
// unreachable from values.yaml. These tests render the chart with
// `helm template` and assert the invariants an operator relies on.
//
// The comparisons are structural rather than golden-file. `go test ./...` runs
// with whatever Helm the machine has, and rendered YAML is formatted by that
// version, so pinning byte-for-byte output would make the suite fail on a Helm
// upgrade instead of on a real regression. The pinned Helm in CI covers lint
// and a plain render.
//
// The tests skip when helm is unavailable, so `go test ./...` stays green on a
// machine without Helm.
package sorobeacon_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// databaseURL is the throwaway connection string every render passes:
// values.schema.json requires database.url or database.existingSecret, and the
// schema is checked before the templates are rendered.
const databaseURL = "postgres://user:pass@host:5432/db"

// chartDir resolves the chart directory relative to this test file, so the
// tests run from any working directory.
func chartDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// helmPath returns the helm binary, skipping the test when it is not installed.
func helmPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed; install Helm (https://helm.sh) to run the chart render tests")
	}
	return p
}

// renderChart runs `helm template` with the given extra flags and returns the
// rendered manifests.
func renderChart(t *testing.T, extraArgs ...string) string {
	t.Helper()
	args := append([]string{
		"template", "sorobeacon", chartDir(),
		"--namespace", "sorobeacon-test",
		"--set", "database.url=" + databaseURL,
	}, extraArgs...)
	out, err := exec.Command(helmPath(t), args...).CombinedOutput()
	require.NoError(t, err, "helm template failed: %s", out)
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

// manifest extracts the first block for the given Kind. Helm separates
// rendered manifests with "---" lines and prefixes each with a Source comment.
// The match is line-anchored so that asking for "Service" does not return the
// ServiceAccount (whose kind line contains "Service" as a prefix).
func manifest(t *testing.T, rendered, kind string) string {
	t.Helper()
	want := "\nkind: " + kind + "\n"
	for _, block := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(block, want) {
			return block
		}
	}
	t.Fatalf("rendered output contains no %s manifest:\n%s", kind, rendered)
	return ""
}

// manifestNamed extracts the manifest block for the given Kind and metadata.name.
func manifestNamed(t *testing.T, rendered, kind, name string) string {
	t.Helper()
	wantKind := "\nkind: " + kind + "\n"
	wantName := "\n  name: " + name + "\n"
	for _, block := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(block, wantKind) && strings.Contains(block, wantName) {
			return block
		}
	}
	t.Fatalf("rendered output contains no %s manifest named %s:\n%s", kind, name, rendered)
	return ""
}

func TestRender_ProbesPointAtTheRealEndpoints(t *testing.T) {
	dep := manifest(t, renderChart(t), "Deployment")

	// A renamed route would make every pod perpetually unhealthy without
	// failing a deploy, so the paths are pinned to the ones the application
	// actually serves.
	for _, want := range []string{
		"startupProbe:", "livenessProbe:", "readinessProbe:",
		"path: /api/v1/livez", "path: /api/v1/readyz",
	} {
		assert.Contains(t, dep, want, "the Deployment must keep its probe contract")
	}
	// Readiness is the only probe allowed to depend on the database and the
	// event source; liveness must stay on the always-200 endpoint.
	assert.NotContains(t, dep, "livenessProbe:\n            httpGet:\n              path: /api/v1/readyz")
}

func TestRender_HardenedSecurityContext(t *testing.T) {
	dep := manifest(t, renderChart(t), "Deployment")

	for _, want := range []string{
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"runAsUser: 10001",
		"runAsGroup: 10001",
		"fsGroup: 10001",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"type: RuntimeDefault",
		"- ALL",
		"mountPath: /tmp",
	} {
		assert.Contains(t, dep, want, "the Deployment must enforce hardened Pod/Container security defaults")
	}
}

func TestRender_HTTPPortMatchesTheService(t *testing.T) {
	rendered := renderChart(t)
	dep := manifest(t, rendered, "Deployment")
	svc := manifest(t, rendered, "Service")
	cm := manifest(t, rendered, "ConfigMap")

	// config.httpAddr has to agree with the container port, or the probes and
	// the Service both address a port nothing is listening on.
	assert.Contains(t, cm, `HTTP_ADDR: ":8080"`)
	assert.Contains(t, dep, "containerPort: 8080")
	assert.Contains(t, svc, "targetPort: http")
	assert.Contains(t, dep, "configMapRef:", "the non-secret configuration must arrive as a ConfigMap")
}

func TestRender_DatabaseURLAlwaysComesFromASecret(t *testing.T) {
	rendered := renderChart(t)
	dep := manifest(t, rendered, "Deployment")

	assert.Contains(t, dep, "- name: DATABASE_URL")
	assert.Contains(t, dep, "secretKeyRef:")
	assert.NotContains(t, dep, databaseURL, "the connection string must never be inlined in the pod spec")
	assert.NotContains(t, dep, `value: "postgres://`, "no env var may carry the connection string as a plain value")
	assert.Contains(t, manifest(t, rendered, "Secret"), "stringData:", "the default render creates the chart-managed Secret")
}

func TestRender_ExistingSecretSuppressesTheChartManagedOne(t *testing.T) {
	rendered := renderChart(t,
		"--set", "database.existingSecret=external-db",
		"--set", "auth.apiToken=",
		"--set", "auth.configEncryptionKey=",
	)

	assert.NotContains(t, rendered, "kind: Secret", "an existingSecret must suppress the chart-managed Secret")
	dep := manifest(t, rendered, "Deployment")
	assert.Contains(t, dep, "name: external-db")
	assert.NotContains(t, dep, databaseURL)
}

// TestRender_EveryConfigEnvVarIsSettable is the "values.yaml covers the whole
// environment" guarantee. The variable list is read from internal/config's
// source rather than hardcoded, so adding a variable there without adding a
// values key and a ConfigMap entry fails this test.
func TestRender_EveryConfigEnvVarIsSettable(t *testing.T) {
	// Render with credentials set so the two secret variables are present too.
	rendered := renderChart(t,
		"--set", "auth.apiToken=test-token",
		"--set", "auth.configEncryptionKey=dGVzdC1rZXk=",
	)

	seen := configEnvVars(t)
	// Guard the scan itself: if a refactor stops it matching, the loop below
	// would pass vacuously.
	for _, must := range []string{
		"DATABASE_URL", "API_TOKEN", "CONFIG_ENCRYPTION_KEY", "NETWORK",
		"RPC_URL", "NETWORK_PASSPHRASE", "POLL_INTERVAL", "LOG_LEVEL",
		"RATE_LIMIT_TRUST_FORWARDED", "ALERT_RETENTION", "DATABASE_MAX_CONNS",
	} {
		require.Truef(t, slices.Contains(seen, must), "scanning internal/config missed %s; the scan pattern needs updating", must)
	}

	for _, name := range seen {
		// ConfigMap/Secret data keys render as `NAME: "value"`; container
		// environment entries render as `- name: NAME`.
		if !strings.Contains(rendered, name+":") && !strings.Contains(rendered, "- name: "+name+"\n") {
			t.Errorf("internal/config reads %s but the chart never sets it; add it to values.yaml", name)
		}
	}
}

// TestRender_ConfigDefaultsMatchTheApplication pins the second half of the
// guarantee: a chart that sets every variable but with a different default is
// still a surprise for anyone who has been running the binary directly.
func TestRender_ConfigDefaultsMatchTheApplication(t *testing.T) {
	cm := manifest(t, renderChart(t), "ConfigMap")
	want := map[string]string{
		"NETWORK":                     "testnet",
		"RPC_URL":                     "https://soroban-testnet.stellar.org",
		"NETWORK_PASSPHRASE":          "",
		"SOURCE_MODE":                 "rpc",
		"SOROTRAIL_URL":               "",
		"HTTP_ADDR":                   ":8080",
		"HTTP_MAX_BODY_BYTES":         "1048576",
		"POLL_INTERVAL":               "5s",
		"LOG_LEVEL":                   "info",
		"MONITOR_SILENT_AFTER":        "24h",
		"SHUTDOWN_GRACE_PERIOD":       "30s",
		"READYZ_LAG_THRESHOLD":        "0",
		"RATE_LIMIT_RPS":              "0",
		"RATE_LIMIT_BURST":            "0",
		"RATE_LIMIT_TRUST_FORWARDED":  "false",
		"CORS_ALLOWED_ORIGINS":        "",
		"ALERT_RETENTION":             "",
		"AUDIT_RETENTION":             "",
		"DATABASE_MAX_CONNS":          "0",
		"DATABASE_MIN_CONNS":          "0",
		"DATABASE_MAX_CONN_LIFETIME":  "",
		"DATABASE_MAX_CONN_IDLE_TIME": "",
	}
	for name, value := range want {
		line := fmt.Sprintf("%s: %q", name, value)
		if !strings.Contains(cm, line) {
			t.Errorf("ConfigMap is missing %s; it must carry the application's own default", line)
		}
	}
}

func TestRender_MigrationHook(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		// The application migrates before it binds its listener, so a hook is
		// opt-in rather than something every release pays for.
		assert.NotContains(t, renderChart(t), "kind: Job")
	})

	t.Run("renders when enabled", func(t *testing.T) {
		job := manifest(t, renderChart(t, "--set", "migrations.enabled=true"), "Job")
		assert.Contains(t, job, `"helm.sh/hook": pre-install,pre-upgrade`)
		assert.Contains(t, job, "/api/v1/livez",
			"the hook must wait for the endpoint that only answers after migrations are applied")
		assert.Contains(t, job, "POLL_INTERVAL")
		assert.Contains(t, job, "/usr/local/bin/sorobeacon",
			"the hook must invoke the binary directly without needing /bin/sh or wget")
		assert.NotContains(t, job, "/bin/sh")
		assert.NotContains(t, job, "wget")
		assert.Contains(t, job, "automountServiceAccountToken: false")
	})

	t.Run("pod is not selectable by the Service", func(t *testing.T) {
		rendered := renderChart(t, "--set", "migrations.enabled=true")
		job := manifest(t, rendered, "Job")
		idx := strings.Index(job, "  template:")
		require.NotEqual(t, -1, idx, "the Job manifest has no pod template")
		pod := job[idx:]
		// A hook pod carrying the Service's selector labels would receive
		// dashboard and API traffic for as long as the hook runs.
		assert.Contains(t, pod, "app.kubernetes.io/name: sorobeacon-migrate")
		assert.NotContains(t, pod, "app.kubernetes.io/name: sorobeacon\n")
	})

	t.Run("uses an existing secret", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "database.existingSecret=external-db",
			"--set", "auth.existingSecret=external-auth",
			"--set", "migrations.enabled=true",
		)
		assert.NotContains(t, rendered, "kind: Secret")
		assert.Contains(t, manifest(t, rendered, "Job"), "name: external-db")
	})
}

func TestRender_SplitHATopologySeparatesAPIAndWorker(t *testing.T) {
	t.Run("combined by default", func(t *testing.T) {
		rendered := renderChart(t)
		assert.NotContains(t, rendered, "name: sorobeacon-worker")
		assert.NotContains(t, rendered, "WORKLOAD_ROLE")
	})

	t.Run("separately scalable API and worker when worker.enabled=true", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "replicaCount=4",
			"--set", "worker.enabled=true",
			"--set", "worker.replicaCount=2",
		)
		apiDep := manifestNamed(t, rendered, "Deployment", "sorobeacon")
		workerDep := manifestNamed(t, rendered, "Deployment", "sorobeacon-worker")
		svc := manifest(t, rendered, "Service")

		assert.Contains(t, apiDep, "replicas: 4")
		assert.Contains(t, apiDep, "--role=api")
		assert.Contains(t, apiDep, "name: WORKLOAD_ROLE\n              value: \"api\"")

		assert.Contains(t, workerDep, "replicas: 2")
		assert.Contains(t, workerDep, "name: WORKLOAD_ROLE\n              value: \"worker\"")
		assert.Contains(t, workerDep, "--role=worker")
		assert.Contains(t, workerDep, "app.kubernetes.io/name: sorobeacon-worker")
		assert.Contains(t, workerDep, "app.kubernetes.io/component: worker")

		// The Service must route external traffic only to API pods, never to
		// background worker pods.
		assert.Contains(t, svc, "app.kubernetes.io/name: sorobeacon\n")
		assert.NotContains(t, svc, "sorobeacon-worker")
	})
}

func TestRender_AutoscalingTargetsAPIOnlyAndIsOffByDefault(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		rendered := renderChart(t)
		assert.NotContains(t, rendered, "kind: HorizontalPodAutoscaler")
		assert.Contains(t, manifest(t, rendered, "Deployment"), "replicas: 1")
	})

	t.Run("scales API Deployment without touching worker replicas", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "autoscaling.enabled=true",
			"--set", "autoscaling.minReplicas=3",
			"--set", "autoscaling.maxReplicas=12",
			"--set", "autoscaling.targetMemoryUtilizationPercentage=70",
			"--set", "autoscaling.targetCPUUtilizationPercentage=80",
			"--set", "worker.enabled=true",
			"--set", "worker.replicaCount=2",
		)
		hpa := manifest(t, rendered, "HorizontalPodAutoscaler")
		apiDep := manifestNamed(t, rendered, "Deployment", "sorobeacon")
		workerDep := manifestNamed(t, rendered, "Deployment", "sorobeacon-worker")

		assert.Contains(t, hpa, "kind: Deployment\n    name: sorobeacon\n")
		assert.NotContains(t, hpa, "sorobeacon-worker", "HPA must never target the lease-bounded worker Deployment")
		assert.Contains(t, hpa, "minReplicas: 3")
		assert.Contains(t, hpa, "maxReplicas: 12")
		assert.Contains(t, hpa, "name: memory")
		assert.Contains(t, hpa, "averageUtilization: 70")
		assert.Contains(t, hpa, "name: cpu")
		assert.Contains(t, hpa, "averageUtilization: 80")
		assert.Contains(t, hpa, "stabilizationWindowSeconds: 300")

		// When HPA is enabled, the API Deployment omits spec.replicas so Helm
		// upgrades do not overwrite the live replica count, while the worker
		// Deployment keeps its explicit replica count.
		assert.NotContains(t, apiDep, "  replicas:")
		assert.Contains(t, workerDep, "  replicas: 2")
	})
}

func TestRender_PodDisruptionBudgetsAreDrainSafe(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		assert.NotContains(t, renderChart(t), "kind: PodDisruptionBudget")
	})

	t.Run("single-replica worker clamps to maxUnavailable 1 so node drains never block", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "replicaCount=1",
			"--set", "podDisruptionBudget.enabled=true",
			"--set", "podDisruptionBudget.minAvailable=1",
			"--set", "worker.enabled=true",
			"--set", "worker.replicaCount=1",
			"--set", "worker.podDisruptionBudget.enabled=true",
			"--set", "worker.podDisruptionBudget.minAvailable=1",
		)
		apiPDB := manifestNamed(t, rendered, "PodDisruptionBudget", "sorobeacon")
		workerPDB := manifestNamed(t, rendered, "PodDisruptionBudget", "sorobeacon-worker")

		assert.Contains(t, apiPDB, "maxUnavailable: 1")
		assert.NotContains(t, apiPDB, "minAvailable:")
		assert.Contains(t, workerPDB, "maxUnavailable: 1", "single-replica worker PDB must allow 1 unavailable pod so kubectl drain succeeds")
		assert.NotContains(t, workerPDB, "minAvailable:")
		assert.Contains(t, workerPDB, "app.kubernetes.io/name: sorobeacon-worker")
	})

	t.Run("multi-replica API and standby worker enforce minAvailable 1", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "replicaCount=3",
			"--set", "podDisruptionBudget.enabled=true",
			"--set", "worker.enabled=true",
			"--set", "worker.replicaCount=2",
			"--set", "worker.podDisruptionBudget.enabled=true",
		)
		apiPDB := manifestNamed(t, rendered, "PodDisruptionBudget", "sorobeacon")
		workerPDB := manifestNamed(t, rendered, "PodDisruptionBudget", "sorobeacon-worker")

		assert.Contains(t, apiPDB, "minAvailable: 1")
		assert.Contains(t, workerPDB, "minAvailable: 1")
	})
}

func TestRender_TerminationGracePeriodAndRollingUpdateReadinessGating(t *testing.T) {
	rendered := renderChart(t, "--set", "worker.enabled=true")
	apiDep := manifestNamed(t, rendered, "Deployment", "sorobeacon")
	workerDep := manifestNamed(t, rendered, "Deployment", "sorobeacon-worker")

	for _, dep := range []string{apiDep, workerDep} {
		assert.Contains(t, dep, "minReadySeconds: 5")
		assert.Contains(t, dep, "terminationGracePeriodSeconds: 60")
		assert.Contains(t, dep, "maxUnavailable: 0")
		assert.Contains(t, dep, "maxSurge: 1")
	}
}

func TestRender_PodAntiAffinityIsOptIn(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		dep := manifest(t, renderChart(t), "Deployment")
		assert.NotContains(t, dep, "podAntiAffinity:")
	})

	t.Run("soft and hard anti-affinity render when enabled", func(t *testing.T) {
		rendered := renderChart(t,
			"--set", "podAntiAffinity.enabled=true",
			"--set", "podAntiAffinity.type=soft",
			"--set", "worker.enabled=true",
			"--set", "worker.podAntiAffinity.enabled=true",
			"--set", "worker.podAntiAffinity.type=hard",
		)
		apiDep := manifestNamed(t, rendered, "Deployment", "sorobeacon")
		workerDep := manifestNamed(t, rendered, "Deployment", "sorobeacon-worker")

		assert.Contains(t, apiDep, "preferredDuringSchedulingIgnoredDuringExecution:")
		assert.Contains(t, apiDep, "topologyKey: kubernetes.io/hostname")
		assert.Contains(t, workerDep, "requiredDuringSchedulingIgnoredDuringExecution:")
		assert.Contains(t, workerDep, "app.kubernetes.io/name: sorobeacon-worker")
	})
}

// configEnvVars scans internal/config's non-test sources for the environment
// variables it reads. It covers the direct reads (os.Getenv) and the three
// helpers the package funnels some of them through.
func configEnvVars(t *testing.T) []string {
	t.Helper()
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`os\.Getenv\("([A-Z0-9_]+)"\)`),
		regexp.MustCompile(`getenv\("([A-Z0-9_]+)"`),
		regexp.MustCompile(`parseInt32Env\("([A-Z0-9_]+)"\)`),
		regexp.MustCompile(`parseDurationEnv\("([A-Z0-9_]+)"\)`),
		// Values that may also come from the config file are read through
		// lookupConfigValue rather than os.Getenv. They are still environment
		// variables as far as the chart is concerned, so they belong in this
		// scan — without this pattern the loop below would find nothing and
		// pass vacuously, which is what the guard on the caller catches.
		regexp.MustCompile(`lookupConfigValue\("([A-Z0-9_]+)"`),
		regexp.MustCompile(`lookupSecret\("([A-Z0-9_]+)"\)`),
		regexp.MustCompile(`valueOrFallback\("([A-Z0-9_]+)"`),
		regexp.MustCompile(`parseInt32EnvWithFile\("([A-Z0-9_]+)"`),
		regexp.MustCompile(`parseDurationEnvWithFile\("([A-Z0-9_]+)"`),
		// The per-network parser reads through a get(name) closure so one
		// function can serve both the NETWORK_* and the NETWORKS_<name>_*
		// forms. Without this pattern RPC_URL and NETWORK_PASSPHRASE go unseen,
		// which the require above is there to catch.
		regexp.MustCompile(`\bget\("([A-Z0-9_]+)"\)`),
	}
	// The chart lives at deploy/helm/sorobeacon, so the config package is
	// three levels up.
	dir := filepath.Join(chartDir(), "..", "..", "..", "internal", "config")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "the chart test expects internal/config next to deploy/")

	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = true
			}
		}
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
