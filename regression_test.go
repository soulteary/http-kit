package httpkit_test

import (
	"os/exec"
	"strings"
	"testing"
)

// nonStandardDeps returns the packages pkg links that are not part of the Go
// standard library.
func nonStandardDeps(t *testing.T, pkg string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}

	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}

	var deps []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps = append(deps, line)
		}
	}
	return deps
}

// TestRootPackageImportsOnlyTheStandardLibrary is the gate on the whole point
// of v2.
//
// Client.InjectTraceContext called otel.GetTextMapPropagator directly, so
// every user of this package linked OpenTelemetry -- four modules and the
// go-logr and cespare/xxhash trees behind them -- whether or not they emitted
// a span. Any import of a third-party package from the root package gives that
// back, and a plain reading of the source will not catch it once it arrives
// three levels down.
func TestRootPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	for _, dep := range nonStandardDeps(t, ".") {
		if dep == "github.com/soulteary/http-kit/v2" {
			continue // the package under test
		}
		t.Errorf("the root package links %s; third-party dependencies belong in a subpackage", dep)
	}
}

// TestOtelpropStillCarriesOpenTelemetry is the other half: the split is only
// worth anything if the functionality moved rather than disappeared.
func TestOtelpropStillCarriesOpenTelemetry(t *testing.T) {
	for _, dep := range nonStandardDeps(t, "./otelprop") {
		if dep == "go.opentelemetry.io/otel" {
			return
		}
	}
	t.Error("otelprop does not link go.opentelemetry.io/otel")
}
