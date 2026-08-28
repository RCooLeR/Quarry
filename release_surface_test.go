package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

func TestTagWorkflowCannotPublishUnsignedArtifacts(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	text := string(workflow)
	for _, forbidden := range []string{
		"contents: write",
		"args: release --clean",
		"gh release",
		"action-gh-release",
		"actions/create-release",
		"upload-release-asset",
		"softprops/action-gh-release",
		"GITHUB_TOKEN:",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("tag workflow contains public-release capability %q", forbidden)
		}
	}
	for _, required := range []string{
		"name: Release candidate validation",
		"permissions:\n  contents: read",
		"name: Confirm public publication is disabled",
		"Validated artifacts remain unqualified non-release CI evidence.",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("tag workflow is missing containment assertion %q", required)
		}
	}
}

func TestGoReleaserPublicationIsDisabled(t *testing.T) {
	config, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatalf("read GoReleaser config: %v", err)
	}
	text := strings.ReplaceAll(string(config), "\r\n", "\n")
	if !strings.Contains(text, "release:\n  disable: true") {
		t.Fatal("GoReleaser must remain unable to publish while release prerequisites are open")
	}
	for _, forbidden := range []string{"draft: false", "mode: append", "extra_files:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("GoReleaser retains active publication setting %q", forbidden)
		}
	}
}

func TestWorkflowActionsUseImmutableRevisions(t *testing.T) {
	workflows, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no GitHub Actions workflows found")
	}
	usesPattern := regexp.MustCompile(`(?m)^\s*uses:\s*([^\s#]+)`)
	pinnedAction := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	for _, path := range workflows {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range usesPattern.FindAllStringSubmatch(string(data), -1) {
			ref := match[1]
			if strings.HasPrefix(ref, "./") {
				continue
			}
			if !pinnedAction.MatchString(ref) {
				t.Errorf("%s uses mutable action reference %q", path, ref)
			}
		}
	}
}

func TestReleaseWorkflowActionPinsAreCurrentAndConsistent(t *testing.T) {
	expected := map[string]string{
		"actions/checkout":             "3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1",
		"actions/setup-go":             "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0",
		"actions/setup-node":           "820762786026740c76f36085b0efc47a31fe5020 # v7.0.0",
		"actions/upload-artifact":      "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1",
		"goreleaser/goreleaser-action": "f06c13b6b1a9625abc9e6e439d9c05a8f2190e94 # v7.2.3",
	}
	seen := make(map[string]bool, len(expected))
	actionLine := regexp.MustCompile(`(?m)^\s*uses:\s*([^@\s#]+)@([0-9a-f]{40})\s+#\s+(\S+)\s*$`)
	for _, path := range []string{".github/workflows/verify.yml", ".github/workflows/release.yml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range actionLine.FindAllStringSubmatch(string(data), -1) {
			want, audited := expected[match[1]]
			if !audited {
				continue
			}
			seen[match[1]] = true
			got := match[2] + " # " + match[3]
			if got != want {
				t.Errorf("%s uses stale or mislabelled %s pin %q; want %q", path, match[1], got, want)
			}
		}
	}
	for action := range expected {
		if !seen[action] {
			t.Errorf("release workflows do not use audited action %s", action)
		}
	}
}

func TestReleaseCandidatesRequireExactTaggedCommitVerification(t *testing.T) {
	releaseData, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	verifyData, err := os.ReadFile(".github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	release := string(releaseData)
	verify := string(verifyData)

	for _, required := range []string{
		"name: Verify annotated tag provenance",
		"-release-tag \"$RELEASE_TAG\"",
		"uses: ./.github/workflows/verify.yml",
		"expected_commit: ${{ needs.provenance.outputs.commit }}",
		"ref: ${{ needs.provenance.outputs.commit }}",
		"name: Upload tag provenance record",
		"retention-days: 7",
		".sha256",
		"UNQUALIFIED-NON-RELEASE-CI-EVIDENCE.txt",
		"name: unqualified-non-release-ci-",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release workflow is missing exact-source gate %q", required)
		}
	}
	for _, job := range []string{"windows", "linux", "publication-contained"} {
		block := workflowJobBlock(t, release, job)
		steps := strings.Index(block, "\n    steps:")
		if steps < 0 {
			t.Fatalf("release job %s has no steps", job)
		}
		dependencies := block[:steps]
		for _, dependency := range []string{"      - provenance", "      - verify"} {
			if !strings.Contains(dependencies, dependency) {
				t.Errorf("release job %s can bypass dependency %q", job, strings.TrimSpace(dependency))
			}
		}
	}
	publication := workflowJobBlock(t, release, "publication-contained")
	for _, dependency := range []string{"      - windows", "      - linux"} {
		if !strings.Contains(publication, dependency) {
			t.Errorf("publication containment job is missing candidate dependency %q", strings.TrimSpace(dependency))
		}
	}
	for _, required := range []string{
		"ref: ${{ inputs.expected_commit }}",
		"test \"${#EXPECTED_COMMIT}\" -eq 40",
		"git status --porcelain --untracked-files=all",
		"name: Static and dependency analysis",
		"name: Record verified source and tools",
	} {
		if !strings.Contains(verify, required) {
			t.Errorf("reusable verification workflow is missing %q", required)
		}
	}
}

func TestUnsupportedMacOSIsAbsentFromVerificationAndCandidateArtifacts(t *testing.T) {
	files := []string{
		".github/workflows/verify.yml",
		".github/workflows/release.yml",
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(data))
		for _, forbidden := range []string{"macos", "darwin"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s still exposes unsupported macOS candidate surface %q", path, forbidden)
			}
		}
	}

	support, err := os.ReadFile("docs/support-and-release.md")
	if err != nil {
		t.Fatal(err)
	}
	supportText := regexp.MustCompile(`\s+`).ReplaceAllString(string(support), " ")
	for _, required := range []string{
		"macOS is excluded",
		"secure atomic output currently fails closed on this platform",
		"No release-candidate build or retained artifact",
		"Every macOS package, application-bundle, signing, and notarization task fails before build, output, credential, or network work",
	} {
		if !strings.Contains(supportText, required) {
			t.Errorf("support policy is missing macOS containment statement %q", required)
		}
	}

	darwinData, err := os.ReadFile("build/darwin/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	darwin := string(darwinData)
	for _, task := range []string{"package", "package:universal", "create:app:bundle", "sign", "sign:notarize"} {
		block := taskfileTaskBlock(t, darwin, task)
		commands := regexp.MustCompile(`(?m)^      - .+$`).FindAllString(block, -1)
		if len(commands) != 1 || strings.TrimSpace(commands[0]) != "- task: unsupported:distribution" {
			t.Errorf("Darwin distribution task %q must invoke only the distribution guard; commands: %q", task, commands)
		}
		if strings.Contains(block, "deps:") {
			t.Errorf("Darwin distribution task %q can perform dependency work before rejecting distribution", task)
		}
	}

	guard := taskfileTaskBlock(t, darwin, "unsupported:distribution")
	for _, required := range []string{
		"internal: true",
		"package/bundle/signing/notarization tasks are disabled",
		"secure atomic output is implemented and qualified on macOS",
		"approved project license and signing/provenance policy",
		"clean-machine install/upgrade/uninstall qualification",
		"exit 1",
	} {
		if !strings.Contains(guard, required) {
			t.Errorf("Darwin distribution guard is missing fail-closed contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"wails3 tool sign",
		"--notarize",
		"-plist-template build/darwin/Info.plist",
		"codesign:adhoc",
		"codesign:skip",
	} {
		if strings.Contains(darwin, forbidden) {
			t.Errorf("Darwin Taskfile retains an unqualified distribution path %q", forbidden)
		}
	}
	for _, task := range []string{"build", "run"} {
		if strings.Contains(taskfileTaskBlock(t, darwin, task), "unsupported:distribution") {
			t.Errorf("raw Darwin %s was incorrectly disabled with the distribution surface", task)
		}
	}

	rootData, err := os.ReadFile("Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	rootPackage := taskfileTaskBlock(t, string(rootData), "package")
	rootCommands := regexp.MustCompile(`(?m)^      - .+$`).FindAllString(rootPackage, -1)
	if len(rootCommands) != 1 || strings.TrimSpace(rootCommands[0]) != `- task: "{{OS}}:package"` || strings.Contains(rootPackage, "deps:") {
		t.Error("root package dispatch can bypass the platform package guard or do dependency work first")
	}
}

func TestWindowsNsisCandidateIsContainedAndDataPreserving(t *testing.T) {
	releaseData, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	release := string(releaseData)
	windowsJob := workflowJobBlock(t, release, "windows")
	for _, required := range []string{
		"Build Windows unqualified non-release candidate",
		"choco install nsis --version=3.11 --allow-downgrade",
		`$actual -ne "v3.11"`,
		"wails3 task windows:package:candidate",
		"verify-unqualified-nsis-candidate.ps1",
		"windows-nsis-candidate-evidence.json",
		`quarry_${env:APP_VERSION}_windows_amd64_nsis.exe`,
		"Unsigned, unqualified Windows installer evidence",
	} {
		if !strings.Contains(windowsJob, required) {
			t.Errorf("Windows candidate workflow is missing %q", required)
		}
	}
	if strings.Contains(windowsJob, "Compress-Archive -Path bin/quarry.exe") {
		t.Error("Windows candidate workflow still retains the raw executable ZIP as its install artifact")
	}

	taskData, err := os.ReadFile("build/windows/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	taskfile := string(taskData)
	candidateTask := taskfileTaskBlock(t, taskfile, "package:candidate")
	for _, required := range []string{
		"unsigned, unqualified Windows 10/11 x64 NSIS validation candidate",
		"ARCH: amd64",
		"INSTALL_SCOPE: machine",
		"APP_VERSION must be supplied from verified release-candidate provenance",
		"PLATFORM_VERSION must be supplied from verified release-candidate provenance",
		"BUILD_NUMBER must be supplied from verified release-candidate provenance",
	} {
		if !strings.Contains(candidateTask, required) {
			t.Errorf("narrow Windows candidate task is missing %q", required)
		}
	}
	installerTask := taskfileTaskBlock(t, taskfile, "create:nsis:installer")
	if !strings.Contains(installerTask, "verify-webview2-bootstrapper.ps1") {
		t.Error("NSIS task can embed the WebView2 bootstrapper without signature verification")
	}
	for _, required := range []string{
		"platforms: [windows]",
		"Windows NSIS construction is Windows-native only",
		"platforms: [linux, darwin]",
	} {
		if !strings.Contains(installerTask, required) {
			t.Errorf("NSIS task does not fail closed outside native Windows: missing %q", required)
		}
	}
	signingTask := taskfileTaskBlock(t, taskfile, "sign:installer")
	for _, required := range []string{
		"intentionally fail-closed",
		"sign the application payload first",
		"exit 1",
	} {
		if !strings.Contains(signingTask, required) {
			t.Errorf("NSIS signing task is missing fail-closed contract %q", required)
		}
	}
	for _, forbidden := range []string{"deps:", "create:nsis:installer", "wails3 tool sign"} {
		if strings.Contains(signingTask, forbidden) {
			t.Errorf("NSIS signing task can still construct or sign a misleading outer-only artifact via %q", forbidden)
		}
	}

	nsisData, err := os.ReadFile("build/windows/nsis/project.nsi")
	if err != nil {
		t.Fatal(err)
	}
	nsis := strings.ReplaceAll(string(nsisData), "\r\n", "\n")
	for _, required := range []string{
		"!macro quarry.requireWebView2",
		`ExecWait '"$pluginsdir\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install' $1`,
		`${If} $1 != 0`,
		"SetErrorLevel 66",
		"Quarry was not installed",
		`Delete "$INSTDIR\${PRODUCT_EXECUTABLE}"`,
		`RMDir "$INSTDIR"`,
	} {
		if !strings.Contains(nsis, required) {
			t.Errorf("NSIS policy is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`!insertmacro wails.webview2runtime`,
		`RMDir /r "$AppData`,
		`RMDir /r $INSTDIR`,
		`RMDir /r "$INSTDIR"`,
	} {
		if strings.Contains(nsis, forbidden) {
			t.Errorf("NSIS policy retains unsafe behavior %q", forbidden)
		}
	}
	if got := strings.Count(nsis, `${If} $0 == "0.0.0.0"`); got != 4 {
		t.Errorf("WebView2 detection must normalize the absent-runtime sentinel after HKLM and HKCU reads both before and after bootstrap; got %d checks", got)
	}
	firstMachineRead := strings.Index(nsis, `ReadRegStr $0 HKLM`)
	firstMachineSentinel := strings.Index(nsis, `${If} $0 == "0.0.0.0"`)
	firstUserRead := strings.Index(nsis, `ReadRegStr $0 HKCU`)
	secondSentinelRelative := -1
	if firstMachineSentinel >= 0 {
		secondSentinelRelative = strings.Index(nsis[firstMachineSentinel+1:], `${If} $0 == "0.0.0.0"`)
		if secondSentinelRelative >= 0 {
			secondSentinelRelative += firstMachineSentinel + 1
		}
	}
	bootstrapExecution := strings.Index(nsis, `ExecWait '"$pluginsdir\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install' $1`)
	if firstMachineRead < 0 || firstMachineSentinel <= firstMachineRead || firstUserRead <= firstMachineSentinel || secondSentinelRelative <= firstUserRead || bootstrapExecution <= secondSentinelRelative {
		t.Error("WebView2 preflight does not normalize the HKLM sentinel before HKCU fallback and the HKCU sentinel before bootstrap")
	}
	installSectionStart := strings.Index(nsis, "\nSection\n")
	uninstallSectionStart := strings.Index(nsis, "\nSection \"uninstall\"")
	if installSectionStart < 0 || uninstallSectionStart <= installSectionStart {
		t.Fatal("could not isolate NSIS install section")
	}
	installSection := nsis[installSectionStart:uninstallSectionStart]
	webviewCheck := strings.Index(installSection, "!insertmacro quarry.requireWebView2")
	copyFiles := strings.Index(installSection, "!insertmacro wails.files")
	if webviewCheck < 0 || copyFiles < 0 || webviewCheck >= copyFiles {
		t.Error("WebView2 prerequisite does not fail before Quarry program files are copied")
	}

	bootstrapScript, err := os.ReadFile("build/windows/verify-webview2-bootstrapper.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Get-AuthenticodeSignature", "SignatureStatus]::Valid", "Microsoft Corporation", "Get-FileHash -Algorithm SHA256"} {
		if !strings.Contains(string(bootstrapScript), required) {
			t.Errorf("WebView2 bootstrap verifier is missing %q", required)
		}
	}
	candidateScript, err := os.ReadFile("build/windows/verify-unqualified-nsis-candidate.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"expected AMD64 (0x8664)",
		"SignatureStatus]::NotSigned",
		"qualificationStatus = \"unqualified-non-release-ci-evidence\"",
		"target = \"windows-10-or-11-x64-validation\"",
		"makensis /VERSION",
	} {
		if !strings.Contains(string(candidateScript), required) {
			t.Errorf("NSIS candidate verifier is missing %q", required)
		}
	}

	configData, err := os.ReadFile("build/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), "fileAssociations: []") {
		t.Error("Windows candidate unexpectedly advertises file associations")
	}
	mainData, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainData), "os.Args") {
		t.Error("main executable gained command-line file opening without updating the candidate contract")
	}

	supportData, err := os.ReadFile("docs/support-and-release.md")
	if err != nil {
		t.Fatal(err)
	}
	support := regexp.MustCompile(`\s+`).ReplaceAllString(string(supportData), " ")
	for _, required := range []string{
		"Windows NSIS validation-candidate contract",
		"not a declaration that either operating system is publicly supported",
		"does not currently register any file association or custom URL protocol",
		"does not accept a source path from the command line",
		"fails or the runtime is still not detectable afterward",
		"removes only the Quarry executable",
		"unknown files found in the install directory",
		"uninstaller has no delete-user-data option",
	} {
		if !strings.Contains(support, required) {
			t.Errorf("Windows candidate documentation is missing %q", required)
		}
	}
}

func TestUnsupportedMobileProductionTasksFailClosed(t *testing.T) {
	rootData, err := os.ReadFile("Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	root := string(rootData)
	for _, forbidden := range []string{"\n  android:", "\n  ios:"} {
		if strings.Contains(root, forbidden) {
			t.Errorf("root task inventory exposes unsupported mobile include %q", strings.TrimSpace(forbidden))
		}
	}

	androidData, err := os.ReadFile("build/android/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	android := string(androidData)
	for _, task := range []string{"package", "package:fat", "assemble:apk:release", "deploy-emulator"} {
		block := taskfileTaskBlock(t, android, task)
		if !strings.Contains(block, "task: unsupported:production") {
			t.Errorf("Android task %q does not fail through the unsupported-production guard", task)
		}
		if strings.Contains(block, "deps:") {
			t.Errorf("Android task %q can perform dependency work before rejecting distribution", task)
		}
	}
	androidBuild := taskfileTaskBlock(t, android, "build")
	if !strings.Contains(androidBuild, "task: ensure:development-only") || strings.Contains(androidBuild, "deps:") {
		t.Error("Android build does not run its production guard before dependency work")
	}
	androidEnsure := taskfileTaskBlock(t, android, "ensure:development-only")
	if !strings.Contains(androidEnsure, `PRODUCTION | default "false"`) || !strings.Contains(androidEnsure, `!= "false"`) || !strings.Contains(androidEnsure, "exit 1") {
		t.Error("Android development-only guard does not reject an explicit production request")
	}
	androidCompile := taskfileTaskBlock(t, android, "compile:go:shared")
	if !strings.Contains(androidCompile, `PRODUCTION | default "false"`) || !strings.Contains(androidCompile, `= "false"`) {
		t.Error("direct Android shared-library compilation does not reject an explicit production request")
	}
	if strings.Contains(android, "-tags production,android") || strings.Contains(android, "assembleRelease") {
		t.Fatal("Android Taskfile retains a production compiler or Gradle release path")
	}
	androidGuard := taskfileTaskBlock(t, android, "unsupported:production")
	for _, required := range []string{"internal: true", "production/package/release tasks are disabled", "exit 1"} {
		if !strings.Contains(androidGuard, required) {
			t.Errorf("Android production guard is missing %q", required)
		}
	}

	gradleData, err := os.ReadFile("build/android/app/build.gradle")
	if err != nil {
		t.Fatal(err)
	}
	gradle := string(gradleData)
	for _, required := range []string{
		`beforeVariants(selector().withBuildType("release"))`,
		"variantBuilder.enable = false",
	} {
		if !strings.Contains(gradle, required) {
			t.Errorf("Android Gradle release containment is missing %q", required)
		}
	}
	for _, forbidden := range []string{"ANDROID_KEYSTORE_FILE", "signingConfigs.debug", "signingConfigs.release"} {
		if strings.Contains(gradle, forbidden) {
			t.Errorf("Android Gradle retains unsafe release-signing path %q", forbidden)
		}
	}
	wrapperData, err := os.ReadFile("build/android/gradle/wrapper/gradle-wrapper.properties")
	if err != nil {
		t.Fatal(err)
	}
	const gradleDistribution = "distributionUrl=https\\://services.gradle.org/distributions/gradle-9.2.1-bin.zip"
	const gradleChecksum = "distributionSha256Sum=72f44c9f8ebcb1af43838f45ee5c4aa9c5444898b3468ab3f4af7b6076c5bc3f"
	for _, required := range []string{gradleDistribution, gradleChecksum, "validateDistributionUrl=true"} {
		if !strings.Contains(string(wrapperData), required) {
			t.Errorf("development-only Android Gradle wrapper is missing pinned download contract %q", required)
		}
	}

	iosData, err := os.ReadFile("build/ios/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	ios := string(iosData)
	for _, task := range []string{"package", "package:ipa", "create:app:bundle", "deploy-simulator", "deploy-device"} {
		block := taskfileTaskBlock(t, ios, task)
		if !strings.Contains(block, "task: unsupported:production") {
			t.Errorf("iOS task %q does not fail through the unsupported-production guard", task)
		}
		if strings.Contains(block, "deps:") {
			t.Errorf("iOS task %q can perform dependency work before rejecting distribution", task)
		}
	}
	iosBuild := taskfileTaskBlock(t, ios, "build")
	if !strings.Contains(iosBuild, "task: ensure:development-only") || strings.Contains(iosBuild, "deps:") {
		t.Error("iOS build does not run its production guard before dependency work")
	}
	iosEnsure := taskfileTaskBlock(t, ios, "ensure:development-only")
	if !strings.Contains(iosEnsure, `PRODUCTION | default "false"`) || !strings.Contains(iosEnsure, `!= "false"`) || !strings.Contains(iosEnsure, "exit 1") {
		t.Error("iOS development-only guard does not reject an explicit production request")
	}
	if strings.Contains(ios, "-tags production,ios") {
		t.Fatal("iOS Taskfile retains a production compiler path")
	}
	iosGuard := taskfileTaskBlock(t, ios, "unsupported:production")
	for _, required := range []string{"internal: true", "production/package/release tasks are disabled", "exit 1"} {
		if !strings.Contains(iosGuard, required) {
			t.Errorf("iOS production guard is missing %q", required)
		}
	}
}

func TestVerificationToolsCannotSelectAnotherGoToolchain(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, required := range []string{
		"name: Run pinned staticcheck\n        env:\n          GOTOOLCHAIN: local",
		"name: Run pinned Go vulnerability scanner\n        env:\n          GOTOOLCHAIN: local",
		"name: Lint GitHub Actions workflows with pinned actionlint\n        env:\n          GOTOOLCHAIN: local",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("verification tool can bypass the repository Go toolchain: missing %q", required)
		}
	}
}

func TestCrossBuildHelperImagesAreImmutable(t *testing.T) {
	const pinnedOwnershipImage = "alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40"
	for _, path := range []string{
		"build/windows/Taskfile.yml",
		"build/linux/Taskfile.yml",
		"build/darwin/Taskfile.yml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "OWNERSHIP_FIX_IMAGE: "+pinnedOwnershipImage) {
			t.Errorf("%s does not pin the cross-build ownership helper by immutable digest", path)
		}
		if strings.Contains(text, ` /app" alpine chown`) || strings.Contains(text, ` /app" alpine:`) {
			t.Errorf("%s retains a mutable inline Alpine helper reference", path)
		}
	}
}

func TestLinuxBuildRejectsCrossArchitectureBeforeBuildWork(t *testing.T) {
	data, err := os.ReadFile("build/linux/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	taskfile := string(data)
	build := taskfileTaskBlock(t, taskfile, "build")

	guard := strings.Index(build, `if [ "{{.TARGET_ARCH}}" != "{{.HOST_ARCH}}" ]; then`)
	dispatch := strings.Index(build, `- task: '{{if`)
	if guard < 0 {
		t.Fatal("linux:build does not reject a target architecture that differs from the host")
	}
	if dispatch < 0 || guard > dispatch {
		t.Fatal("linux:build architecture guard must run before native/Docker build dispatch")
	}
	for _, required := range []string{
		"Linux cross-architecture builds are disabled",
		"matching native GTK/WebKit libraries and GCC",
		"exit 1",
		"HOST_ARCH: '{{ARCH}}'",
		"ARCH: '{{.TARGET_ARCH}}'",
	} {
		if !strings.Contains(build, required) {
			t.Errorf("linux:build architecture guard is missing %q", required)
		}
	}

	dockerBuild := taskfileTaskBlock(t, taskfile, "build:docker")
	for _, required := range []string{
		`[ "{{.DOCKER_ARCH}}" = "{{.HOST_ARCH}}" ]`,
		"image uses native GCC and GTK/WebKit libraries",
		"HOST_ARCH: '{{ARCH}}'",
		"DOCKER_ARCH: '{{.ARCH | default ARCH}}'",
		`docker image inspect --format="{{"{{"}}.Architecture{{"}}"}}" "{{.CROSS_IMAGE}}"`,
		`= "{{.DOCKER_ARCH}}"`,
		"require image '{{.CROSS_IMAGE}}' itself to use architecture",
	} {
		if !strings.Contains(dockerBuild, required) {
			t.Errorf("Linux Docker fallback can bypass architecture containment: missing %q", required)
		}
	}
}

func TestLinuxDockerArchitectureGuardExecutesMatchAndMismatch(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	data, err := os.ReadFile("build/linux/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	var guardTemplate string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "docker image inspect --format=") {
			const prefix = "- sh: '"
			if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "'") {
				t.Fatalf("Docker architecture guard must remain a single-quoted YAML shell scalar: %q", line)
			}
			guardTemplate = strings.TrimSuffix(strings.TrimPrefix(line, prefix), "'")
			break
		}
	}
	if guardTemplate == "" {
		t.Fatal("Docker architecture guard shell command not found")
	}
	tmpl, err := template.New("docker-architecture-guard").Option("missingkey=error").Parse(guardTemplate)
	if err != nil {
		t.Fatalf("parse Docker architecture Task template: %v", err)
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, map[string]string{"CROSS_IMAGE": "wails-cross", "DOCKER_ARCH": "amd64"}); err != nil {
		t.Fatalf("render Docker architecture Task template: %v", err)
	}
	const wantGuard = `test "$(docker image inspect --format="{{.Architecture}}" "wails-cross")" = "amd64"`
	if rendered.String() != wantGuard {
		t.Fatalf("Docker architecture Task template renders %q; want %q", rendered.String(), wantGuard)
	}
	t.Logf("rendered guard: %s", rendered.String())

	for _, test := range []struct {
		imageArchitecture string
		wantSuccess       bool
	}{
		{imageArchitecture: "amd64", wantSuccess: true},
		{imageArchitecture: "arm64", wantSuccess: false},
	} {
		command := `docker() { printf '%s\n' "$FAKE_DOCKER_ARCH"; }; ` + rendered.String()
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(os.Environ(), "FAKE_DOCKER_ARCH="+test.imageArchitecture)
		err := cmd.Run()
		if (err == nil) != test.wantSuccess {
			t.Errorf("guard with image architecture %q returned %v; want success=%v", test.imageArchitecture, err, test.wantSuccess)
		}
	}
}

func TestCrossImageRejectsMissingOrUnsupportedBuildArchitecture(t *testing.T) {
	data, err := os.ReadFile("build/docker/Dockerfile.cross")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	for _, required := range []string{
		`case "${TARGETARCH}" in`,
		"amd64|arm64)",
		"unsupported or missing Docker TARGETARCH",
		`amd64) echo "x86_64"`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("cross image can silently choose the wrong native architecture: missing %q", required)
		}
	}
	if strings.Contains(dockerfile, `*) echo "x86_64"`) {
		t.Error("cross image still defaults an unknown Docker TARGETARCH to amd64")
	}
}

func TestVerificationRecordNamesOnlyExecutedGates(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/verify.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	for _, required := range []string{
		`"windows-tests-build"`,
		`"linux-tests-build"`,
		`"linux-production-tests"`,
		`"linux-race-tests"`,
		`"frontend-tests-build-windows"`,
		`"frontend-tests-build-linux"`,
		`"go-vet-host-and-windows"`,
		`"staticcheck-host-and-windows"`,
		`"govulncheck-host-and-windows"`,
		`"npm-audit-high"`,
		`"actionlint"`,
		`"goreleaser-containment-check"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("verification record omits executed gate %s", required)
		}
	}
	for _, misleading := range []string{`"windows-analysis"`, `"frontend"`, `"race"`} {
		if strings.Contains(workflow, misleading) {
			t.Errorf("verification record retains ambiguous or nonexistent gate %s", misleading)
		}
	}
}

func TestLinuxDistributionTasksFailClosed(t *testing.T) {
	data, err := os.ReadFile("build/linux/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	taskfile := string(data)

	for _, task := range []string{
		"package",
		"create:appimage",
		"create:deb",
		"create:rpm",
		"create:aur",
		"generate:deb",
		"generate:rpm",
		"generate:aur",
		"sign:deb",
		"sign:rpm",
		"sign:packages",
	} {
		block := taskfileTaskBlock(t, taskfile, task)
		if !strings.Contains(block, "task: unsupported:distribution") {
			t.Errorf("Linux distribution task %q does not fail through the distribution guard", task)
		}
		if strings.Contains(block, "deps:") {
			t.Errorf("Linux distribution task %q can perform dependency work before rejecting distribution", task)
		}
	}

	guard := taskfileTaskBlock(t, taskfile, "unsupported:distribution")
	for _, required := range []string{
		"internal: true",
		"package/signing tasks are disabled",
		"immutable digest",
		"approved project license",
		"clean-machine install/upgrade/uninstall qualification",
		"exit 1",
	} {
		if !strings.Contains(guard, required) {
			t.Errorf("Linux distribution guard is missing fail-closed contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"wails3 generate appimage",
		"wails3 tool package",
		"wails3 tool sign",
		"PGP_KEY",
	} {
		if strings.Contains(taskfile, forbidden) {
			t.Errorf("Linux Taskfile retains an unqualified distribution path %q", forbidden)
		}
	}

	build := taskfileTaskBlock(t, taskfile, "build")
	if strings.Contains(build, "unsupported:distribution") {
		t.Error("raw Linux build was incorrectly disabled with the distribution surface")
	}

	scriptData, err := os.ReadFile("build/linux/appimage/build.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptData)
	for _, required := range []string{
		"Linux AppImage packaging is disabled",
		"immutable, verified tool digest",
		"clean-machine package qualification",
		"exit 1",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("AppImage helper is missing fail-closed contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"/continuous/",
		"wget ",
		"curl ",
		"mkdir ",
		"cp ",
		"mv ",
		"chmod +x",
		"--output appimage",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("AppImage helper retains a download-or-execute path %q", forbidden)
		}
	}
}

func TestLocalBuildAndPackageSurfacesDoNotClaimReleaseQualification(t *testing.T) {
	for _, path := range []string{
		"Taskfile.yml",
		"build/windows/Taskfile.yml",
		"build/linux/Taskfile.yml",
		"build/darwin/Taskfile.yml",
		"README.md",
		"docs/getting-started.md",
		"docs/development.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(data))
		for _, forbidden := range []string{
			"packages a production build",
			"packaged production build",
			"# production build",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("%s makes the unqualified local build/package claim %q", path, forbidden)
			}
		}
	}
}

func workflowJobBlock(t *testing.T, workflow, job string) string {
	t.Helper()
	marker := "\n  " + job + ":\n"
	start := strings.Index(workflow, marker)
	if start < 0 {
		t.Fatalf("workflow job %q not found", job)
	}
	start += 1
	rest := workflow[start:]
	nextJob := regexp.MustCompile(`(?m)^  [a-zA-Z0-9_-]+:\n`).FindStringIndex(rest[len("  "+job+":\n"):])
	if nextJob == nil {
		return rest
	}
	return rest[:len("  "+job+":\n")+nextJob[0]]
}

func taskfileTaskBlock(t *testing.T, taskfile, task string) string {
	t.Helper()
	marker := "\n  " + task + ":\n"
	start := strings.Index(taskfile, marker)
	if start < 0 {
		t.Fatalf("task %q not found", task)
	}
	start++
	rest := taskfile[start:]
	headerLength := len("  " + task + ":\n")
	nextTask := regexp.MustCompile(`(?m)^  [a-zA-Z0-9:_-]+:\n`).FindStringIndex(rest[headerLength:])
	if nextTask == nil {
		return rest
	}
	return rest[:headerLength+nextTask[0]]
}

func TestPrivilegedWebviewSecurityGateRunsForDevelopmentAndChecksNavigation(t *testing.T) {
	data, err := os.ReadFile("frontend/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	const securityCheck = "node scripts/check-privileged-webview.mjs"
	if dev := manifest.Scripts["dev"]; !strings.HasPrefix(dev, securityCheck+" && ") {
		t.Fatalf("frontend dev server can start without the privileged-WebView gate: %q", dev)
	}

	checkData, err := os.ReadFile("frontend/scripts/check-privileged-webview.mjs")
	if err != nil {
		t.Fatal(err)
	}
	check := string(checkData)
	for _, required := range []string{
		`join(sourceRoot, "main.tsx")`,
		"installNavigationPolicy",
		"does not import the privileged WebView navigation policy",
		"does not install the privileged WebView navigation policy before rendering",
	} {
		if !strings.Contains(check, required) {
			t.Errorf("privileged-WebView check does not enforce navigation bootstrap contract %q", required)
		}
	}
}

func TestPinnedReleaseToolchainIsConsistent(t *testing.T) {
	type packageManifest struct {
		PackageManager  string            `json:"packageManager"`
		Engines         map[string]string `json:"engines"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	data, err := os.ReadFile("frontend/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.PackageManager != "npm@12.0.2" || manifest.Engines["npm"] != "12.0.2" {
		t.Fatalf("npm pin is inconsistent: packageManager=%q engines=%q", manifest.PackageManager, manifest.Engines["npm"])
	}
	// Keep the Wails frontend runtime on the exact protocol peer selected by the
	// Go module and CLI. Arbitrarily mixing prerelease generations is not a
	// compatibility policy for the desktop bridge.
	if manifest.Dependencies["@wailsio/runtime"] != "3.0.0-beta.15" {
		t.Fatalf("Wails frontend runtime must remain exact, got %q", manifest.Dependencies["@wailsio/runtime"])
	}
	for group, dependencies := range map[string]map[string]string{
		"dependencies":    manifest.Dependencies,
		"devDependencies": manifest.DevDependencies,
	} {
		for name, version := range dependencies {
			if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`).MatchString(version) {
				t.Errorf("%s %s is not pinned exactly: %q", group, name, version)
			}
		}
	}
	nodeVersion, err := os.ReadFile(".node-version")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(nodeVersion)) != "24.20.0" {
		t.Fatalf(".node-version = %q", nodeVersion)
	}

	files := []string{
		"go.mod",
		"Taskfile.yml",
		".github/workflows/verify.yml",
		".github/workflows/release.yml",
		"build/docker/Dockerfile.cross",
		"build/windows/Taskfile.yml",
		"build/linux/Taskfile.yml",
		"build/darwin/Taskfile.yml",
	}
	var combined strings.Builder
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(content)
	}
	text := combined.String()
	for _, required := range []string{
		"go 1.27.0",
		"GOTOOLCHAIN: local",
		"node-version: 24.20.0",
		"npm --version)\" = \"12.0.2",
		"github.com/wailsapp/wails/v3 v3.0.0-beta.15",
		"github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.15",
		"honnef.co/go/tools/cmd/staticcheck@v0.8.1",
		"golang.org/x/vuln/cmd/govulncheck@v1.7.0",
		"github.com/rhysd/actionlint/cmd/actionlint@v1.7.12",
		"version: v2.18.0",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release toolchain is missing exact pin %q", required)
		}
	}
}

func TestDependencyUpdatePolicyAndCrossImagePin(t *testing.T) {
	dependabot, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, ecosystem := range []string{"gomod", "npm", "github-actions", "docker"} {
		if !strings.Contains(string(dependabot), "package-ecosystem: "+ecosystem) {
			t.Errorf("Dependabot does not cover %s", ecosystem)
		}
	}

	dockerfile, err := os.ReadFile("build/docker/Dockerfile.cross")
	if err != nil {
		t.Fatal(err)
	}
	from := regexp.MustCompile(`(?m)^FROM\s+[^\s@]+@sha256:[0-9a-f]{64}\s*$`)
	if !from.Match(dockerfile) {
		t.Fatal("cross-build container base must use a content digest, not a mutable tag alone")
	}
	const crossBase = "FROM golang:1.27.0-trixie@sha256:ae28539d2ef595b9a2930dd7f031d9592376829dc0eae7cb869559f7d5812c3a"
	if !bytes.Contains(dockerfile, []byte(crossBase)) {
		t.Fatalf("cross-build container does not use the qualified Go 1.27 base %q", crossBase)
	}
	if bytes.Contains(dockerfile, []byte("go install mvdan.cc/garble")) {
		t.Fatal("cross-build image still installs a Garble release that rejects Go 1.27")
	}
	for _, required := range []string{
		"OBFUSCATED",
		"no tagged Garble release supports Go 1.27",
		"exit 1",
		"ARG ZIG_VERSION=0.16.0",
		"zig-${ZIG_ARCH}-linux-${ZIG_VERSION}.tar.xz",
		"70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00",
		"ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17",
		"ARG MACOS_SDK_VERSION=26.1",
		"ARG MACOS_SDK_SHA256=beee7212d265a6d2867d0236cc069314b38d5fb3486a6515734e76fa210c784c",
		"pkg-config --atleast-version=4.14 gtk4",
		"pkg-config --exists webkitgtk-6.0",
		"-mmacos-version-min=13.0",
		"export MACOSX_DEPLOYMENT_TARGET=13.0",
	} {
		if !bytes.Contains(dockerfile, []byte(required)) {
			t.Errorf("cross-build image is missing Go 1.27 build requirement %q", required)
		}
	}
	if bytes.Contains(dockerfile, []byte("-mmacosx-version-min=12.0")) || bytes.Contains(dockerfile, []byte("-mmacos-version-min=12.0")) {
		t.Error("cross-build image still targets unsupported macOS 12")
	}
}

func TestGo127DarwinMinimumIsConsistent(t *testing.T) {
	taskData, err := os.ReadFile("build/darwin/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	task := string(taskData)
	for _, required := range []string{
		`CGO_CFLAGS: "-mmacosx-version-min=13.0"`,
		`CGO_LDFLAGS: "-mmacosx-version-min=13.0"`,
		`MACOSX_DEPLOYMENT_TARGET: "13.0"`,
		"no tagged Garble release supports Go 1.27",
	} {
		if !strings.Contains(task, required) {
			t.Errorf("Darwin build surface is missing Go 1.27 requirement %q", required)
		}
	}
	if strings.Contains(task, "mmacosx-version-min=12.0") || strings.Contains(task, `MACOSX_DEPLOYMENT_TARGET: "12.0"`) {
		t.Error("Darwin build surface still targets unsupported macOS 12")
	}

	for _, path := range []string{"build/darwin/Info.plist", "build/darwin/Info.dev.plist"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata := string(data)
		if !strings.Contains(metadata, "<key>LSMinimumSystemVersion</key>") || !strings.Contains(metadata, "<string>13.0.0</string>") {
			t.Errorf("%s does not declare macOS 13 as its minimum", path)
		}
		if strings.Contains(metadata, "<string>12.0.0</string>") {
			t.Errorf("%s still declares unsupported macOS 12", path)
		}
	}
}

func TestBuildAssetUpdaterReappliesDarwinMinimum(t *testing.T) {
	data, err := os.ReadFile("build/Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	block := taskfileTaskBlock(t, string(data), "update:build-assets")
	upstream := strings.Index(block, "wails3 update build-assets")
	policy := strings.Index(block, "go run ./assetpolicy")
	if upstream < 0 || policy < 0 || policy <= upstream {
		t.Fatalf("build-asset policy must run after the Wails updater:\n%s", block)
	}
	for _, required := range []string{
		`MACOS_MINIMUM: "13.0.0"`,
		`-macos-minimum "{{.MACOS_MINIMUM}}"`,
		"darwin/Info.plist",
		"darwin/Info.dev.plist",
	} {
		if !strings.Contains(block, required) {
			t.Errorf("build-asset updater does not preserve the macOS floor: missing %q", required)
		}
	}
}
