package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input    string
		full     string
		platform string
	}{
		{input: "v1.2.3", full: "1.2.3", platform: "1.2.3"},
		{input: "2.0.0-rc.1+build.7", full: "2.0.0-rc.1+build.7", platform: "2.0.0"},
		{input: "0.0.0-dev", full: "0.0.0-dev", platform: "0.0.0"},
	} {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseVersion(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Full != tc.full || got.Platform != tc.platform {
				t.Fatalf("parseVersion(%q) = %+v", tc.input, got)
			}
		})
	}
}

func TestParseVersionRejectsInvalidOrUnrepresentableValues(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "v1", "1.2.3.4", "01.2.3", "1.2.3-01", "65536.0.0", "release-1.2.3", "1.2.3\nBAD=1"} {
		if _, err := parseVersion(input); err == nil {
			t.Errorf("parseVersion(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseBuildNumber(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"1", "42", "65535"} {
		got, err := parseBuildNumber(input)
		if err != nil || got != input {
			t.Errorf("parseBuildNumber(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"0", "01", "65536", "rc1", "1\nBAD=1"} {
		if _, err := parseBuildNumber(input); err == nil {
			t.Errorf("parseBuildNumber(%q) unexpectedly succeeded", input)
		}
	}
}

func TestWriteWindowsInfoUsesFullAndPlatformVersions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	template := filepath.Join(dir, "info.json")
	out := filepath.Join(dir, "generated", "info.json")
	if err := os.WriteFile(template, []byte(`{"fixed":{"file_version":"0.0.0"},"info":{"0000":{"ProductVersion":"0.0.0","ProductName":"Quarry"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	version, _ := parseVersion("v1.4.0-rc.2")
	version.BuildNumber = "27"
	if err := writeWindowsInfo(template, out, version); err != nil {
		t.Fatal(err)
	}
	var got windowsInfo
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Fixed["file_version"] != "1.4.0.27" || got.Fixed["product_version"] != "1.4.0.27" || got.Info["0000"]["ProductVersion"] != "1.4.0-rc.2" || got.Info["0000"]["FileVersion"] != "1.4.0-rc.2" {
		t.Fatalf("generated metadata = %+v", got)
	}
}

func TestWriteWindowsManifestUsesNumericFourPartVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	template := filepath.Join(dir, "app.manifest")
	out := filepath.Join(dir, "generated", "app.manifest")
	content := `<assembly><assemblyIdentity type="win32" name="com.rcooler.quarry" version="0.0.0.0" processorArchitecture="*"/><dependency><assemblyIdentity type="win32" name="dependency" version="6.0.0.0"/></dependency></assembly>`
	if err := os.WriteFile(template, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	version, _ := parseVersion("v2.3.4-beta.1")
	version.BuildNumber = "91"
	if err := writeWindowsManifest(template, out, version); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `name="com.rcooler.quarry" version="2.3.4.91"`) || !strings.Contains(string(data), `name="dependency" version="6.0.0.0"`) {
		t.Fatalf("generated manifest = %s", data)
	}
}

func TestWriteDarwinPlistUsesPlatformVersionAndRejectsMissingKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	template := filepath.Join(dir, "Info.plist")
	out := filepath.Join(dir, "generated", "Info.plist")
	content := `<plist><dict><key>CFBundleShortVersionString</key><string>0.0.0</string><key>CFBundleVersion</key><string>0.0.0</string></dict></plist>`
	if err := os.WriteFile(template, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	version, _ := parseVersion("v3.2.1-beta.1")
	version.BuildNumber = "314"
	if err := writeDarwinPlist(template, out, version); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<key>CFBundleShortVersionString</key><string>3.2.1</string>") || !strings.Contains(string(data), "<key>CFBundleVersion</key><string>314</string>") {
		t.Fatalf("generated plist = %s", data)
	}
	if err := os.WriteFile(template, []byte(`<plist><dict></dict></plist>`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDarwinPlist(template, out, version); err == nil {
		t.Fatal("missing version keys unexpectedly succeeded")
	}
}

func TestWriteGitHubEnvRejectsMultilineInjection(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	version, _ := parseVersion("v1.2.3")
	version.BuildNumber = "88"
	if err := writeGitHubEnv(path, version, "abc\nBAD=1", "2026-07-12T10:00:00Z"); err == nil {
		t.Fatal("multiline commit unexpectedly succeeded")
	}
	commit := strings.Repeat("a", 40)
	if err := writeGitHubEnv(path, version, commit, "2026-07-12T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "APP_VERSION=1.2.3\nPLATFORM_VERSION=1.2.3\nBUILD_NUMBER=88\nBUILD_COMMIT="+commit+"\nBUILD_DATE=2026-07-12T10:00:00Z\n" {
		t.Fatalf("environment output = %q", data)
	}
}

func TestInspectReleaseTagBindsAnnotatedTagToCleanHead(t *testing.T) {
	repo := newGitRepository(t)
	runGit(t, repo, "tag", "-a", "v1.2.3-rc.1", "-m", "release candidate")

	version, source, err := inspectReleaseTag(repo, "v1.2.3-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if version.Full != "1.2.3-rc.1" || version.Platform != "1.2.3" {
		t.Fatalf("version = %+v", version)
	}
	if source.Tag != "v1.2.3-rc.1" || !objectIDPattern.MatchString(source.TagObject) || !objectIDPattern.MatchString(source.Commit) || !objectIDPattern.MatchString(source.Tree) {
		t.Fatalf("source = %+v", source)
	}
	if source.BuildDate != "2026-07-12T10:00:00Z" || source.SourceDateEpoch <= 0 {
		t.Fatalf("source date = %+v", source)
	}
}

func TestInspectReleaseTagRejectsLightweightDirtyOrMismatchedSources(t *testing.T) {
	t.Run("lightweight", func(t *testing.T) {
		repo := newGitRepository(t)
		runGit(t, repo, "tag", "v1.2.3")
		if _, _, err := inspectReleaseTag(repo, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "annotated") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		repo := newGitRepository(t)
		runGit(t, repo, "tag", "-a", "v1.2.3", "-m", "release")
		if err := os.WriteFile(filepath.Join(repo, "untracked"), []byte("dirty"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := inspectReleaseTag(repo, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "not clean") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("head mismatch", func(t *testing.T) {
		repo := newGitRepository(t)
		runGit(t, repo, "tag", "-a", "v1.2.3", "-m", "release")
		if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("second\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "source.txt")
		runGit(t, repo, "commit", "-m", "second")
		if _, _, err := inspectReleaseTag(repo, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunWritesVerifiedGitHubOutputsAndProvenance(t *testing.T) {
	repo := newGitRepository(t)
	runGit(t, repo, "tag", "-a", "v2.4.6", "-m", "release")
	dir := t.TempDir()
	outputs := filepath.Join(dir, "github-output")
	if err := os.WriteFile(outputs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	provenance := filepath.Join(dir, "release-provenance.json")
	if err := run([]string{
		"-release-tag", "v2.4.6",
		"-repository", repo,
		"-build-number", "27",
		"-github-output", outputs,
		"-provenance-out", provenance,
	}); err != nil {
		t.Fatal(err)
	}

	outputData, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"app_version=2.4.6\n",
		"platform_version=2.4.6\n",
		"build_number=27\n",
		"build_commit=",
		"build_tree=",
		"source_date_epoch=",
		"tag_object=",
	} {
		if !strings.Contains(string(outputData), required) {
			t.Errorf("GitHub outputs missing %q: %s", required, outputData)
		}
	}

	var record releaseProvenance
	provenanceData, err := os.ReadFile(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(provenanceData, &record); err != nil {
		t.Fatal(err)
	}
	if record.SchemaVersion != 1 || record.Tag != "v2.4.6" || record.Version != "2.4.6" || record.BuildNumber != "27" || !objectIDPattern.MatchString(record.Commit) {
		t.Fatalf("provenance = %+v", record)
	}
}

func newGitRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.name", "Quarry Test")
	runGit(t, repo, "config", "user.email", "quarry-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "source.txt")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-07-12T10:00:00Z",
		"GIT_COMMITTER_DATE=2026-07-12T10:00:00Z",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
