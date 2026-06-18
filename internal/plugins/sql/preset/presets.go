package preset

import (
	"errors"
	"fmt"
	"strings"

	replacepkg "github.com/quarry/quarry-wails3/internal/replace"
)

type Mode string

const (
	ModePlain Mode = "plain"
	ModeRegex Mode = "regex"
	ModeBatch Mode = "batch"
)

const (
	RemoveDefinerPreset       = "Remove DEFINER"
	ChangeDatabasePreset      = "Change database name"
	ChangeCharsetPreset       = "Change charset/collation"
	ConvertEnginePreset       = "Convert MyISAM to InnoDB"
	RemoveAutoIncrementPreset = "Remove AUTO_INCREMENT"
)

var PresetNames = []string{
	RemoveDefinerPreset,
	ChangeDatabasePreset,
	ChangeCharsetPreset,
	ConvertEnginePreset,
	RemoveAutoIncrementPreset,
}

type Config struct {
	Mode          Mode
	Search        string
	Replace       string
	Regex         bool
	CaseSensitive bool
	WholeWord     bool
	BatchRules    []replacepkg.BatchRule
	Summary       string
}

func Build(name string, arg1 string, arg2 string, arg3 string, arg4 string) (Config, error) {
	switch name {
	case RemoveDefinerPreset:
		return Config{
			Mode:          ModeRegex,
			Search:        definerRegex(),
			Replace:       "",
			Regex:         true,
			CaseSensitive: true,
			Summary:       "Remove MySQL DEFINER clauses, including backtick, single-quoted, and unquoted forms, with bounded regex replace.",
		}, nil
	case ChangeDatabasePreset:
		if arg1 == "" || arg2 == "" {
			return Config{}, errors.New("database preset needs old and new database names")
		}
		rules := databaseNameRules(arg1, arg2)
		return Config{
			Mode:          ModeBatch,
			BatchRules:    rules,
			CaseSensitive: true,
			Summary:       fmt.Sprintf("Replace common database-name forms from %q to %q.", arg1, arg2),
		}, nil
	case ChangeCharsetPreset:
		if arg1 == "" || arg2 == "" {
			return Config{}, errors.New("charset preset needs old and new charset values")
		}
		rules := []replacepkg.BatchRule{
			{Name: "CHARSET", Find: []byte("CHARSET=" + arg1), Replace: []byte("CHARSET=" + arg2), Priority: 0},
			{Name: "DEFAULT CHARSET", Find: []byte("DEFAULT CHARSET=" + arg1), Replace: []byte("DEFAULT CHARSET=" + arg2), Priority: 1},
			{Name: "SET NAMES", Find: []byte("SET NAMES " + arg1), Replace: []byte("SET NAMES " + arg2), Priority: 2},
		}
		if arg3 != "" || arg4 != "" {
			if arg3 == "" || arg4 == "" {
				return Config{}, errors.New("collation preset needs both old and new collation values")
			}
			rules = append(rules,
				replacepkg.BatchRule{Name: "COLLATE", Find: []byte("COLLATE=" + arg3), Replace: []byte("COLLATE=" + arg4), Priority: 3},
				replacepkg.BatchRule{Name: "COLLATE spaced", Find: []byte("COLLATE " + arg3), Replace: []byte("COLLATE " + arg4), Priority: 4},
			)
		}
		return Config{
			Mode:          ModeBatch,
			BatchRules:    rules,
			CaseSensitive: true,
			Summary:       "Rewrite SQL charset and optional collation declarations.",
		}, nil
	case ConvertEnginePreset:
		return Config{
			Mode: ModeBatch,
			BatchRules: []replacepkg.BatchRule{
				{Name: "ENGINE", Find: []byte("ENGINE=MyISAM"), Replace: []byte("ENGINE=InnoDB"), Priority: 0},
				{Name: "ENGINE spaced", Find: []byte("ENGINE = MyISAM"), Replace: []byte("ENGINE = InnoDB"), Priority: 1},
			},
			CaseSensitive: true,
			Summary:       "Convert common MyISAM engine declarations to InnoDB.",
		}, nil
	case RemoveAutoIncrementPreset:
		return Config{
			Mode:          ModeRegex,
			Search:        `\s+AUTO_INCREMENT=\d+`,
			Replace:       "",
			Regex:         true,
			CaseSensitive: true,
			Summary:       "Remove AUTO_INCREMENT table options.",
		}, nil
	default:
		return Config{}, errors.New("unknown SQL preset")
	}
}

func definerRegex() string {
	backtickIdent := "`(?:``|[^`])*`"
	singleQuotedIdent := `'(?:''|\\'|[^'])*'`
	unquotedIdent := `[^\s@]+`
	ident := `(?:` + backtickIdent + `|` + singleQuotedIdent + `|` + unquotedIdent + `)`
	return `\s*DEFINER\s*=\s*` + ident + `\s*@\s*` + ident
}

func databaseNameRules(oldName string, newName string) []replacepkg.BatchRule {
	rules := []replacepkg.BatchRule{
		{
			Name:     "Backtick identifiers",
			Find:     []byte(quoteBacktickIdentifier(oldName)),
			Replace:  []byte(quoteBacktickIdentifier(newName)),
			Priority: 0,
		},
	}
	if bareSQLIdentifier(oldName) && bareSQLIdentifier(newName) {
		rules = append(rules,
			replacepkg.BatchRule{Name: "Qualified names", Find: []byte(oldName + "."), Replace: []byte(newName + "."), Priority: len(rules)},
			replacepkg.BatchRule{Name: "USE statements", Find: []byte("USE " + oldName), Replace: []byte("USE " + newName), Priority: len(rules) + 1},
		)
	}
	return rules
}

func quoteBacktickIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func bareSQLIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r == '_' || r == '$' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		return false
	}
	return true
}
