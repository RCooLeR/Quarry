package preset

import "errors"

// ErrDisabled is returned before producing any byte-level SQL replacement
// configuration. Such rules cannot prove that SQL literals and embedded
// PHP/WordPress serialized values remain structurally valid.
var ErrDisabled = errors.New("SQL cleanup presets are disabled until token-aware transformations preserve structured and serialized values")

// Config is intentionally empty while the legacy preset builder is contained.
// Keeping the return type allows old internal callers to fail closed instead of
// silently receiving executable replacement rules.
type Config struct{}

// Build is a retained fail-closed compatibility boundary. It never returns raw
// find/replace or regex rules.
func Build(name string, arg1 string, arg2 string, arg3 string, arg4 string) (Config, error) {
	return Config{}, ErrDisabled
}
