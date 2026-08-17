// Command releasemeta validates a canonical semantic version and writes the
// platform metadata used by release builds. Generated files belong under the
// ignored .task directory so normal builds never rewrite tracked templates.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var objectIDPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

type releaseVersion struct {
	Full     string
	Platform string
	// BuildNumber is the monotonically increasing release-workflow run number.
	// It remains numeric so it can be represented by both Windows fixed-file
	// metadata and Apple's CFBundleVersion.
	BuildNumber string
}

type gitReleaseSource struct {
	Tag             string
	TagObject       string
	Commit          string
	Tree            string
	BuildDate       string
	SourceDateEpoch int64
}

type releaseProvenance struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Tag             string `json:"tag"`
	TagObject       string `json:"tagObject"`
	Commit          string `json:"commit"`
	Tree            string `json:"tree"`
	Version         string `json:"version"`
	PlatformVersion string `json:"platformVersion"`
	BuildNumber     string `json:"buildNumber"`
	BuildDate       string `json:"buildDate"`
	SourceDateEpoch int64  `json:"sourceDateEpoch"`
}

func (v releaseVersion) windowsFileVersion() string {
	if v.BuildNumber == "" {
		return v.Platform
	}
	return v.Platform + "." + v.BuildNumber
}

func (v releaseVersion) windowsManifestVersion() string {
	if v.BuildNumber == "" {
		return v.Platform + ".0"
	}
	return v.Platform + "." + v.BuildNumber
}

func (v releaseVersion) darwinBuildVersion() string {
	if v.BuildNumber == "" {
		return v.Platform
	}
	return v.BuildNumber
}

func parseVersion(raw string) (releaseVersion, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	matches := semverPattern.FindStringSubmatch(raw)
	if matches == nil {
		return releaseVersion{}, fmt.Errorf("version %q is not canonical semantic version MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]", raw)
	}
	if matches[4] != "" {
		for _, identifier := range strings.Split(matches[4], ".") {
			if isDecimal(identifier) && len(identifier) > 1 && identifier[0] == '0' {
				return releaseVersion{}, fmt.Errorf("version %q has a prerelease numeric identifier with a leading zero", raw)
			}
		}
	}
	for _, component := range matches[1:4] {
		n, err := strconv.ParseUint(component, 10, 16)
		if err != nil || n > 65535 {
			return releaseVersion{}, fmt.Errorf("version component %q exceeds the Windows metadata limit 65535", component)
		}
	}
	return releaseVersion{
		Full:     raw,
		Platform: strings.Join(matches[1:4], "."),
	}, nil
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseBuildNumber(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !isDecimal(raw) || (len(raw) > 1 && raw[0] == '0') {
		return "", fmt.Errorf("build number %q must be a canonical positive decimal integer", raw)
	}
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || n == 0 || n > 65535 {
		return "", fmt.Errorf("build number %q must be between 1 and 65535", raw)
	}
	return raw, nil
}

func inspectReleaseTag(repository, tag string) (releaseVersion, gitReleaseSource, error) {
	if repository == "" {
		repository = "."
	}
	version, err := parseVersion(tag)
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, err
	}
	if tag != "v"+version.Full {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release tag %q must be exactly v followed by its canonical semantic version", tag)
	}

	tagRef := "refs/tags/" + tag
	tagObject, err := gitOutput(repository, "rev-parse", "--verify", tagRef+"^{tag}")
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release tag %q must be an annotated tag: %w", tag, err)
	}
	commit, err := gitOutput(repository, "rev-parse", "--verify", tagRef+"^{commit}")
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("resolve release tag commit: %w", err)
	}
	head, err := gitOutput(repository, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("resolve checked-out commit: %w", err)
	}
	if commit != head {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release tag commit %s does not match checked-out commit %s", commit, head)
	}
	status, err := gitOutput(repository, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("inspect release worktree: %w", err)
	}
	if status != "" {
		return releaseVersion{}, gitReleaseSource{}, errors.New("release worktree is not clean")
	}
	tree, err := gitOutput(repository, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("resolve release tree: %w", err)
	}
	buildDate, err := gitOutput(repository, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("read release commit date: %w", err)
	}
	parsedBuildDate, err := time.Parse(time.RFC3339, buildDate)
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release commit date %q is not RFC3339: %w", buildDate, err)
	}
	buildDate = parsedBuildDate.UTC().Format(time.RFC3339)
	epochText, err := gitOutput(repository, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("read release commit timestamp: %w", err)
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil || epoch < 0 {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release commit timestamp %q is invalid", epochText)
	}
	if parsedBuildDate.Unix() != epoch {
		return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release commit date %q and timestamp %q disagree", buildDate, epochText)
	}
	for label, value := range map[string]string{
		"tag object": tagObject,
		"commit":     commit,
		"tree":       tree,
	} {
		if !objectIDPattern.MatchString(value) {
			return releaseVersion{}, gitReleaseSource{}, fmt.Errorf("release %s %q is not a full Git object ID", label, value)
		}
	}

	return version, gitReleaseSource{
		Tag:             tag,
		TagObject:       tagObject,
		Commit:          commit,
		Tree:            tree,
		BuildDate:       buildDate,
		SourceDateEpoch: epoch,
	}, nil
}

func gitOutput(repository string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repository
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

type windowsInfo struct {
	Fixed map[string]string            `json:"fixed"`
	Info  map[string]map[string]string `json:"info"`
}

func writeWindowsInfo(templatePath, outputPath string, version releaseVersion) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read Windows metadata template: %w", err)
	}
	var info windowsInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("decode Windows metadata template: %w", err)
	}
	if info.Fixed == nil || len(info.Info) == 0 {
		return errors.New("windows metadata template is missing fixed or localized info fields")
	}
	info.Fixed["file_version"] = version.windowsFileVersion()
	info.Fixed["product_version"] = version.windowsFileVersion()
	for _, localized := range info.Info {
		localized["ProductVersion"] = version.Full
		localized["FileVersion"] = version.Full
	}
	encoded, err := json.MarshalIndent(info, "", "\t")
	if err != nil {
		return fmt.Errorf("encode Windows metadata: %w", err)
	}
	encoded = append(encoded, '\n')
	return writeGenerated(outputPath, encoded)
}

func writeWindowsManifest(templatePath, outputPath string, version releaseVersion) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read Windows manifest template: %w", err)
	}
	text := string(data)
	start := strings.Index(text, "<assemblyIdentity")
	if start < 0 {
		return errors.New("windows manifest template is missing the application assemblyIdentity")
	}
	relativeEnd := strings.IndexByte(text[start:], '>')
	if relativeEnd < 0 {
		return errors.New("windows manifest template has an unterminated application assemblyIdentity")
	}
	end := start + relativeEnd + 1
	tag := text[start:end]
	if !strings.Contains(tag, `type="win32"`) || !strings.Contains(tag, `name=`) {
		return errors.New("windows manifest application assemblyIdentity is malformed")
	}
	versionPattern := regexp.MustCompile(`\bversion="[^"]*"`)
	if len(versionPattern.FindAllStringIndex(tag, -1)) != 1 {
		return errors.New("windows manifest application assemblyIdentity must contain exactly one version")
	}
	tag = versionPattern.ReplaceAllString(tag, `version="`+version.windowsManifestVersion()+`"`)
	text = text[:start] + tag + text[end:]
	return writeGenerated(outputPath, []byte(text))
}

func writeDarwinPlist(templatePath, outputPath string, version releaseVersion) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read macOS metadata template: %w", err)
	}
	text := string(data)
	var replaceErr error
	values := map[string]string{
		"CFBundleShortVersionString": version.Platform,
		"CFBundleVersion":            version.darwinBuildVersion(),
	}
	for _, key := range []string{"CFBundleShortVersionString", "CFBundleVersion"} {
		pattern := regexp.MustCompile(`(?s)(<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>)[^<]*(</string>)`)
		if !pattern.MatchString(text) {
			replaceErr = errors.Join(replaceErr, fmt.Errorf("macOS metadata template is missing %s", key))
			continue
		}
		text = pattern.ReplaceAllString(text, `${1}`+values[key]+`${2}`)
	}
	if replaceErr != nil {
		return replaceErr
	}
	return writeGenerated(outputPath, []byte(text))
}

func writeGitHubEnv(path string, version releaseVersion, commit, buildDate string) error {
	if version.BuildNumber == "" {
		return errors.New("release build number is required for GitHub release metadata")
	}
	commit = strings.TrimSpace(commit)
	buildDate = strings.TrimSpace(buildDate)
	if !objectIDPattern.MatchString(commit) {
		return errors.New("release commit must be a full lowercase Git object ID")
	}
	if _, err := time.Parse(time.RFC3339, buildDate); err != nil {
		return errors.New("release build date must be a single RFC3339 timestamp")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open GitHub environment file: %w", err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "APP_VERSION=%s\nPLATFORM_VERSION=%s\nBUILD_NUMBER=%s\nBUILD_COMMIT=%s\nBUILD_DATE=%s\n", version.Full, version.Platform, version.BuildNumber, commit, buildDate)
	if err != nil {
		return fmt.Errorf("write GitHub environment file: %w", err)
	}
	return f.Sync()
}

func writeGitHubOutput(path string, version releaseVersion, source gitReleaseSource) error {
	if version.BuildNumber == "" {
		return errors.New("release build number is required for GitHub release metadata")
	}
	values := []struct {
		key   string
		value string
	}{
		{key: "app_version", value: version.Full},
		{key: "platform_version", value: version.Platform},
		{key: "build_number", value: version.BuildNumber},
		{key: "build_commit", value: source.Commit},
		{key: "build_tree", value: source.Tree},
		{key: "build_date", value: source.BuildDate},
		{key: "source_date_epoch", value: strconv.FormatInt(source.SourceDateEpoch, 10)},
		{key: "tag_object", value: source.TagObject},
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open GitHub output file: %w", err)
	}
	defer f.Close()
	for _, item := range values {
		if item.value == "" || strings.ContainsAny(item.value, "\r\n") {
			return fmt.Errorf("GitHub output %s must be a non-empty single line", item.key)
		}
		if _, err := fmt.Fprintf(f, "%s=%s\n", item.key, item.value); err != nil {
			return fmt.Errorf("write GitHub output file: %w", err)
		}
	}
	return f.Sync()
}

func writeReleaseProvenance(path string, version releaseVersion, source gitReleaseSource) error {
	record := releaseProvenance{
		SchemaVersion:   1,
		Tag:             source.Tag,
		TagObject:       source.TagObject,
		Commit:          source.Commit,
		Tree:            source.Tree,
		Version:         version.Full,
		PlatformVersion: version.Platform,
		BuildNumber:     version.BuildNumber,
		BuildDate:       source.BuildDate,
		SourceDateEpoch: source.SourceDateEpoch,
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode release provenance: %w", err)
	}
	return writeGenerated(path, append(encoded, '\n'))
}

func writeGenerated(path string, data []byte) error {
	if path == "" {
		return errors.New("generated metadata output path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create generated metadata directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write generated metadata: %w", err)
	}
	return nil
}

func run(args []string) error {
	set := flag.NewFlagSet("releasemeta", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	versionArg := set.String("version", "", "semantic version or v-prefixed release tag")
	releaseTag := set.String("release-tag", "", "annotated v-prefixed tag to verify against the clean checked-out commit")
	repository := set.String("repository", ".", "Git repository used with -release-tag")
	buildNumberArg := set.String("build-number", "", "monotonic numeric release build number")
	windowsTemplate := set.String("windows-template", "", "tracked Windows info.json template")
	windowsOut := set.String("windows-out", "", "generated Windows info.json output")
	windowsManifestTemplate := set.String("windows-manifest-template", "", "tracked Windows application manifest template")
	windowsManifestOut := set.String("windows-manifest-out", "", "generated Windows application manifest output")
	plistTemplate := set.String("plist-template", "", "tracked macOS Info.plist template")
	plistOut := set.String("plist-out", "", "generated macOS Info.plist output")
	githubEnv := set.String("github-env", "", "GitHub Actions environment file to append")
	githubOutput := set.String("github-output", "", "GitHub Actions step-output file to append verified tag metadata")
	provenanceOut := set.String("provenance-out", "", "JSON release provenance output (requires -release-tag)")
	commit := set.String("commit", "", "release commit (required with -github-env)")
	buildDate := set.String("build-date", "", "reproducible commit date (required with -github-env)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	var source gitReleaseSource
	var version releaseVersion
	var err error
	if *releaseTag != "" {
		version, source, err = inspectReleaseTag(*repository, *releaseTag)
		if err != nil {
			return err
		}
		if *versionArg != "" {
			explicitVersion, parseErr := parseVersion(*versionArg)
			if parseErr != nil {
				return parseErr
			}
			if explicitVersion.Full != version.Full {
				return fmt.Errorf("explicit version %q does not match release tag %q", explicitVersion.Full, *releaseTag)
			}
		}
		if *commit != "" || *buildDate != "" {
			return errors.New("-commit and -build-date cannot override metadata derived with -release-tag")
		}
		*commit = source.Commit
		*buildDate = source.BuildDate
	} else {
		version, err = parseVersion(*versionArg)
		if err != nil {
			return err
		}
	}
	version.BuildNumber, err = parseBuildNumber(*buildNumberArg)
	if err != nil {
		return err
	}
	requested := 0
	if *windowsTemplate != "" || *windowsOut != "" {
		requested++
		if *windowsTemplate == "" || *windowsOut == "" {
			return errors.New("-windows-template and -windows-out must be supplied together")
		}
		if err := writeWindowsInfo(*windowsTemplate, *windowsOut, version); err != nil {
			return err
		}
	}
	if *windowsManifestTemplate != "" || *windowsManifestOut != "" {
		requested++
		if *windowsManifestTemplate == "" || *windowsManifestOut == "" {
			return errors.New("-windows-manifest-template and -windows-manifest-out must be supplied together")
		}
		if err := writeWindowsManifest(*windowsManifestTemplate, *windowsManifestOut, version); err != nil {
			return err
		}
	}
	if *plistTemplate != "" || *plistOut != "" {
		requested++
		if *plistTemplate == "" || *plistOut == "" {
			return errors.New("-plist-template and -plist-out must be supplied together")
		}
		if err := writeDarwinPlist(*plistTemplate, *plistOut, version); err != nil {
			return err
		}
	}
	if *githubEnv != "" {
		requested++
		if err := writeGitHubEnv(*githubEnv, version, *commit, *buildDate); err != nil {
			return err
		}
	}
	if *githubOutput != "" {
		requested++
		if *releaseTag == "" {
			return errors.New("-github-output requires -release-tag so outputs are bound to Git")
		}
		if err := writeGitHubOutput(*githubOutput, version, source); err != nil {
			return err
		}
	}
	if *provenanceOut != "" {
		requested++
		if *releaseTag == "" {
			return errors.New("-provenance-out requires -release-tag")
		}
		if err := writeReleaseProvenance(*provenanceOut, version, source); err != nil {
			return err
		}
	}
	if requested == 0 {
		return errors.New("no output requested")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasemeta:", err)
		os.Exit(2)
	}
}
