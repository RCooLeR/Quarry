package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyMinimumRewritesOnlyDeclaredVersion(t *testing.T) {
	t.Parallel()
	input := []byte(`<?xml version="1.0"?><plist><dict><key>LSMinimumSystemVersion</key>
	<string>12.0.0</string><key>Unrelated</key><string>12.0.0</string></dict></plist>`)
	want := strings.Replace(string(input), "<string>12.0.0</string>", "<string>13.0.0</string>", 1)

	got, err := applyMinimum(input, "13.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("updated plist = %q; want %q", got, want)
	}

	got, err = applyMinimum(got, "13.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("idempotent update = %q; want %q", got, want)
	}
}

func TestApplyMinimumRejectsAmbiguousOrInvalidInput(t *testing.T) {
	t.Parallel()
	valid := `<plist><dict><key>LSMinimumSystemVersion</key><string>12.0.0</string></dict></plist>`
	for name, input := range map[string]string{
		"missing":   `<plist><dict></dict></plist>`,
		"duplicate": valid + valid,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := applyMinimum([]byte(input), "13.0.0"); err == nil {
				t.Fatal("invalid plist unexpectedly succeeded")
			}
		})
	}
	for _, minimum := range []string{"", "13", "13.0", "013.0.0", "13.0.0<bad>"} {
		if _, err := applyMinimum([]byte(valid), minimum); err == nil {
			t.Errorf("invalid minimum %q unexpectedly succeeded", minimum)
		}
	}
}

func TestEnforceMinimumValidatesEveryTargetBeforeWriting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := filepath.Join(dir, "Info.plist")
	second := filepath.Join(dir, "Info.dev.plist")
	original := []byte(`<plist><dict><key>LSMinimumSystemVersion</key><string>12.0.0</string></dict></plist>`)
	if err := os.WriteFile(first, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(`<plist><dict></dict></plist>`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := enforceMinimum([]string{first, second}, "13.0.0"); err == nil {
		t.Fatal("invalid second plist unexpectedly succeeded")
	}
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("first plist changed before all targets validated: %q", got)
	}

	if err := os.WriteFile(second, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enforceMinimum([]string{first, second}, "13.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "<string>13.0.0</string>") {
			t.Errorf("%s does not contain enforced minimum: %s", path, data)
		}
	}
}

func TestEnforceMinimumRejectsOversizedPlist(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "Info.plist")
	data := make([]byte, maxPlistSize+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	err := enforceMinimum([]string{path}, "13.0.0")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized plist error = %v; want size-limit failure", err)
	}
}

func TestRunRequiresPolicyAndTargets(t *testing.T) {
	t.Parallel()
	if err := run(nil); err == nil {
		t.Fatal("missing policy unexpectedly succeeded")
	}
	if err := run([]string{"-macos-minimum", "13.0.0"}); err == nil {
		t.Fatal("missing targets unexpectedly succeeded")
	}
}
