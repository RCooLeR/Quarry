package preset

import (
	"regexp"
	"testing"
)

func TestBuildRemoveDefinerPreset(t *testing.T) {
	cfg, err := Build(RemoveDefinerPreset, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeRegex || !cfg.Regex {
		t.Fatalf("mode = %q regex=%v", cfg.Mode, cfg.Regex)
	}
	if cfg.Search == "" {
		t.Fatal("expected regex search text")
	}
}

func TestBuildRemoveDefinerPresetMatchesQuotedForms(t *testing.T) {
	cfg, err := Build(RemoveDefinerPreset, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	re, err := regexp.Compile(cfg.Search)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"CREATE DEFINER=`root`@`localhost` VIEW v AS SELECT 1",
		"CREATE DEFINER='root'@'localhost' VIEW v AS SELECT 1",
		"CREATE DEFINER='weird''user'@'local''host' VIEW v AS SELECT 1",
		"CREATE DEFINER=root@localhost VIEW v AS SELECT 1",
	}
	for _, tc := range cases {
		if !re.MatchString(tc) {
			t.Fatalf("expected DEFINER regex to match %q", tc)
		}
	}
}

func TestBuildChangeDatabasePreset(t *testing.T) {
	cfg, err := Build(ChangeDatabasePreset, "old_db", "new_db", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeBatch {
		t.Fatalf("mode = %q", cfg.Mode)
	}
	if len(cfg.BatchRules) != 3 {
		t.Fatalf("len(rules) = %d, want 3", len(cfg.BatchRules))
	}
	if string(cfg.BatchRules[0].Find) != "`old_db`" || string(cfg.BatchRules[0].Replace) != "`new_db`" {
		t.Fatalf("first rule = %q => %q", cfg.BatchRules[0].Find, cfg.BatchRules[0].Replace)
	}
}

func TestBuildChangeDatabasePresetEscapesBacktickNames(t *testing.T) {
	cfg, err := Build(ChangeDatabasePreset, "old`db", "new`db", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeBatch {
		t.Fatalf("mode = %q", cfg.Mode)
	}
	if len(cfg.BatchRules) != 1 {
		t.Fatalf("len(rules) = %d, want only quoted rule for non-bare names", len(cfg.BatchRules))
	}
	if string(cfg.BatchRules[0].Find) != "`old``db`" || string(cfg.BatchRules[0].Replace) != "`new``db`" {
		t.Fatalf("first rule = %q => %q", cfg.BatchRules[0].Find, cfg.BatchRules[0].Replace)
	}
}

func TestBuildChangeCharsetPresetRequiresCollationPair(t *testing.T) {
	if _, err := Build(ChangeCharsetPreset, "utf8mb4", "utf8", "utf8mb4_unicode_ci", ""); err == nil {
		t.Fatal("expected collation pair validation")
	}
}

func TestBuildChangeCharsetPresetWithCollation(t *testing.T) {
	cfg, err := Build(ChangeCharsetPreset, "utf8mb4", "utf8", "utf8mb4_unicode_ci", "utf8_unicode_ci")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeBatch {
		t.Fatalf("mode = %q", cfg.Mode)
	}
	if len(cfg.BatchRules) != 5 {
		t.Fatalf("len(rules) = %d, want 5", len(cfg.BatchRules))
	}
}

func TestBuildConvertEnginePreset(t *testing.T) {
	cfg, err := Build(ConvertEnginePreset, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeBatch || len(cfg.BatchRules) != 2 {
		t.Fatalf("mode=%q rules=%d", cfg.Mode, len(cfg.BatchRules))
	}
}

func TestBuildRemoveAutoIncrementPreset(t *testing.T) {
	cfg, err := Build(RemoveAutoIncrementPreset, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeRegex || cfg.Replace != "" {
		t.Fatalf("mode=%q replace=%q", cfg.Mode, cfg.Replace)
	}
}
