package preset

import (
	"errors"
	"testing"
)

func TestBuildAlwaysFailsClosedWithoutReplacementRules(t *testing.T) {
	for _, test := range []struct {
		name string
		args [5]string
	}{
		{name: "remove definer", args: [5]string{"Remove DEFINER"}},
		{name: "change database", args: [5]string{"Change database name", "old", "new"}},
		{name: "serialized value bait", args: [5]string{"Change database name", `s:3:"old";`, "longer"}},
		{name: "unknown", args: [5]string{"future preset"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Build(test.args[0], test.args[1], test.args[2], test.args[3], test.args[4])
			if cfg != (Config{}) || !errors.Is(err, ErrDisabled) {
				t.Fatalf("Build = %#v, %v; want zero config/ErrDisabled", cfg, err)
			}
		})
	}
}
