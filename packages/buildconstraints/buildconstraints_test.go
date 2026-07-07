package buildconstraints

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type listedPackage struct {
	ImportPath string
	Imports    []string
	Deps       []string
	Error      *packageError
	DepsErrors []packageError
}

type packageError struct {
	ImportStack []string
	Pos         string
	Err         string
}

func TestMatrixDefaultBuildStaysFreeOfE2EECrypto(t *testing.T) {
	pkg := goListOne(t, nil, "./packages/agent/modes/matrix")

	forbiddenImports := []string{
		"maunium.net/go/mautrix/crypto/cryptohelper",
		"maunium.net/go/mautrix/crypto",
		"maunium.net/go/mautrix/crypto/libolm",
	}
	for _, forbidden := range forbiddenImports {
		if slices.Contains(pkg.Imports, forbidden) {
			t.Fatalf("default Matrix package directly imports %s; keep E2EE-only dependencies behind the goolm build tag", forbidden)
		}
		if slices.Contains(pkg.Deps, forbidden) {
			t.Fatalf("default Matrix package transitively depends on %s; keep E2EE-only dependencies behind the goolm build tag", forbidden)
		}
	}
}

func TestDefaultCommandBuildIsCGOIndependent(t *testing.T) {
	pkgs := goListDeps(t, []string{"CGO_ENABLED=0"}, "./cmd/zot")

	var failures []string
	for _, pkg := range pkgs {
		if pkg.Error != nil {
			failures = append(failures, pkg.ImportPath+": "+pkg.Error.Err)
		}
		for _, depErr := range pkg.DepsErrors {
			failures = append(failures, pkg.ImportPath+": "+depErr.Err)
		}
	}
	if len(failures) > 0 {
		t.Fatalf("CGO-disabled default command build has package errors:\n%s", strings.Join(failures, "\n"))
	}
}

func goListOne(t *testing.T, env []string, pkg string) listedPackage {
	t.Helper()
	pkgs := goList(t, env, "-e", "-json", pkg)
	if len(pkgs) != 1 {
		t.Fatalf("go list %s returned %d packages, want 1", pkg, len(pkgs))
	}
	return pkgs[0]
}

func goListDeps(t *testing.T, env []string, pkg string) []listedPackage {
	t.Helper()
	return goList(t, env, "-deps", "-e", "-json", pkg)
}

func goList(t *testing.T, env []string, args ...string) []listedPackage {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list %s failed:\n%s", strings.Join(args, " "), string(exitErr.Stderr))
		}
		t.Fatalf("go list %s failed: %v", strings.Join(args, " "), err)
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []listedPackage
	for {
		var pkg listedPackage
		err := dec.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
