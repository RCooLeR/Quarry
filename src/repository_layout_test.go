package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRepositoryKeepsImplementationUnderSrc(t *testing.T) {
	// Go tests run in the module directory, src/. Keep module-relative assets
	// together while repository policy and documentation remain one level up.
	for _, path := range []string{
		"go.mod", "go.sum", "main.go", "Taskfile.yml",
		"internal", "frontend/package.json", "frontend/src", "build/config.yml",
		"../README.md", "../AGENTS.md", "../docs", "../.github", "../Taskfile.yml",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("required source-layout path %q: %v", path, err)
		}
	}
	for _, path := range []string{"go.mod", "go.sum", "internal", "frontend", "build"} {
		if _, err := os.Stat(filepath.Join("..", path)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("implementation path %q must exist only under src; root stat: %v", path, err)
		}
	}
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			t.Errorf("Go source %q must be under src, not the repository root", entry.Name())
		}
	}
}

func TestCIUsesSrcModuleAndRepositoryLevelPolicies(t *testing.T) {
	for _, path := range []string{"../.github/workflows/verify.yml", "../.github/workflows/release.yml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		workflow := strings.ReplaceAll(string(data), "\r\n", "\n")
		for _, required := range []string{
			"go-version-file: src/go.mod",
			"cache-dependency-path: src/go.sum",
			"cache-dependency-path: src/frontend/package-lock.json",
		} {
			if !strings.Contains(workflow, required) {
				t.Errorf("%s is missing relocated dependency input %q", path, required)
			}
		}
		for _, obsolete := range []string{
			"go-version-file: go.mod",
			"cache-dependency-path: go.sum",
			"cache-dependency-path: frontend/package-lock.json",
			"working-directory: frontend",
		} {
			if strings.Contains(workflow, obsolete) {
				t.Errorf("%s still uses root-level source path %q", path, obsolete)
			}
		}
		if strings.HasSuffix(path, "/verify.yml") {
			for _, job := range []string{"platform", "analysis", "verification-record"} {
				block := workflowJobBlock(t, workflow, job)
				if !strings.Contains(block, "defaults:\n      run:\n        working-directory: src") {
					t.Errorf("verification job %q must execute module commands in src", job)
				}
			}
			if !strings.Contains(workflow, "name: Lint GitHub Actions workflows with pinned actionlint\n        working-directory: .") {
				t.Error("workflow linter must execute at the repository root")
			}
		} else {
			provenance := workflowJobBlock(t, workflow, "provenance")
			if !strings.Contains(provenance, "working-directory: src") || !strings.Contains(provenance, "-repository ..") {
				t.Error("release provenance must run the src tool against the repository root")
			}
		}
	}

	data, err := os.ReadFile("../.github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	policy := strings.ReplaceAll(string(data), "\r\n", "\n")
	for ecosystem, directory := range map[string]string{
		"gomod": "/src", "npm": "/src/frontend", "docker": "/src/build/docker", "github-actions": "/",
	} {
		if !strings.Contains(policy, "package-ecosystem: "+ecosystem+"\n    directory: "+directory+"\n") {
			t.Errorf("Dependabot %s must track %s", ecosystem, directory)
		}
	}
}

func TestBuildOutputsStayOutsideSourceWorkspace(t *testing.T) {
	for path, required := range map[string][]string{
		"Taskfile.yml": {`BIN_DIR: "../bin"`},
		"build/docker/Dockerfile.cross": {
			"WORKDIR /app/src",
			"mkdir -p /app/bin",
			`-o "/app/bin/${APP}-${GOOS}-${GOARCH}${EXT}"`,
		},
		"build/windows/nsis/project.nsi": {
			`OutFile "..\..\..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe"`,
		},
		"build/ios/project.pbxproj": {
			`path = "../../../../bin/Quarry.a";`,
			`-o \"../bin/Quarry.a\"`,
		},
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range required {
			if !strings.Contains(string(data), contract) {
				t.Errorf("%s is missing repository-level build-output contract %q", path, contract)
			}
		}
	}
}

func TestCIAnalysisPreparesDesktopDependenciesAndEmbeddedFrontend(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	analysis := workflowJobBlock(t, strings.ReplaceAll(string(data), "\r\n", "\n"), "analysis")
	previous := -1
	for _, prerequisite := range []string{
		"libgtk-4-dev libwebkitgtk-6.0-dev",
		"libgtk-3-dev libwebkit2gtk-4.1-dev",
		"go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.23",
		"run: wails3 task frontend:check",
		"go vet ./...",
	} {
		index := strings.Index(analysis, prerequisite)
		if index <= previous {
			t.Fatalf("analysis must prepare desktop/embedded assets before whole-module vet; missing or misplaced %q", prerequisite)
		}
		previous = index
	}
}

func TestLocalWorkflowGateUsesRepositoryRoot(t *testing.T) {
	data, err := os.ReadFile("Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	block := taskfileTaskBlock(t, strings.ReplaceAll(string(data), "\r\n", "\n"), "verify:workflows")
	match := regexp.MustCompile(`(?m)^    dir: (.+)$`).FindStringSubmatch(block)
	if match == nil {
		t.Fatal("workflow gate must explicitly select the repository directory outside src")
	}
	gateDirectory, err := os.Stat(strings.Trim(match[1], `'"`))
	if err != nil {
		t.Fatalf("resolve source-relative workflow gate directory: %v", err)
	}
	repositoryDirectory, err := os.Stat("..")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gateDirectory, repositoryDirectory) {
		t.Fatal("workflow gate must run at the repository root, not the source module")
	}
}
