package version

import (
	"testing"
)

func TestConstraintMatches(t *testing.T) {
	tests := []struct {
		constraint string
		version    string
		want       bool
	}{
		// Exact match.
		{"1.0.0", "1.0.0", true},
		{"1.0.0", "1.0.1", false},
		{"1.0.0", "2.0.0", false},

		// Greater than / less than.
		{">1.0.0", "1.0.1", true},
		{">1.0.0", "1.0.0", false},
		{">1.0.0", "0.9.0", false},
		{">=1.0.0", "1.0.0", true},
		{">=1.0.0", "0.9.0", false},
		{"<2.0.0", "1.9.9", true},
		{"<2.0.0", "2.0.0", false},
		{"<=2.0.0", "2.0.0", true},
		{"<=2.0.0", "2.0.1", false},
		{"!=1.0.0", "1.0.1", true},
		{"!=1.0.0", "1.0.0", false},

		// Caret (^) — allows changes that do not modify the left-most non-zero digit.
		{"^1.2.3", "1.2.3", true},
		{"^1.2.3", "1.9.9", true},
		{"^1.2.3", "1.2.2", false},
		{"^1.2.3", "2.0.0", false},
		{"^0.3.0", "0.3.0", true},
		{"^0.3.0", "0.3.9", true},
		{"^0.3.0", "0.4.0", false},
		{"^0.0.3", "0.0.3", true},
		{"^0.0.3", "0.0.4", false},

		// Tilde (~) — allows patch-level changes (or minor if only major.minor given).
		{"~1.2.3", "1.2.3", true},
		{"~1.2.3", "1.2.9", true},
		{"~1.2.3", "1.3.0", false},
		{"~1.2", "1.2.0", true},
		{"~1.2", "1.9.9", true},
		{"~1.2", "2.0.0", false},

		// Wildcard (*).
		{"1.0.*", "1.0.0", true},
		{"1.0.*", "1.0.99", true},
		{"1.0.*", "1.1.0", false},
		{"1.*", "1.0.0", true},
		{"1.*", "1.99.99", true},
		{"1.*", "2.0.0", false},
		{"*", "99.99.99", true},

		// AND (space-separated).
		{">=1.0.0 <2.0.0", "1.5.0", true},
		{">=1.0.0 <2.0.0", "2.0.0", false},
		{">=1.0.0 <2.0.0", "0.9.0", false},
		{">=1.0 <1.5", "1.3.0", true},
		{">=1.0 <1.5", "1.5.0", false},

		// OR (||).
		{"^1.0 || ^2.0", "1.5.0", true},
		{"^1.0 || ^2.0", "2.3.0", true},
		{"^1.0 || ^2.0", "3.0.0", false},
		{"^1.0|^2.0", "1.5.0", true},
		{"^1.0|^2.0", "2.3.0", true},
		{"^1.0|^2.0", "3.0.0", false},
		{"1.0.0 || 2.0.0", "1.0.0", true},
		{"1.0.0 || 2.0.0", "2.0.0", true},
		{"1.0.0 || 2.0.0", "3.0.0", false},

		// Comma as AND (Composer allows both space and comma).
		{">=1.0.0,<2.0.0", "1.5.0", true},
		{">=1.0.0,<2.0.0", "2.0.0", false},

		// Stability in constraints.
		{">=1.0.0-beta1", "1.0.0-beta1", true},
		{">=1.0.0-beta1", "1.0.0-beta2", true},
		{">=1.0.0-beta1", "1.0.0", true},
		{">=1.0.0-beta1", "1.0.0-alpha1", false},

		// Dev version matching.
		{"dev-main", "dev-main", true},
		{"dev-main", "dev-master", false},

		// Pre-release matching (Composer compat: unsuffixed >=/caret/tilde match pre-releases).
		{"^6.0.0", "6.0.0-alpha1", true},
		{"^6.0.0", "6.0.0", true},
		{"^6.0.0", "7.0.0-alpha1", false},
		{"^6.0.0", "5.9.0", false},
		{"~6.0.0", "6.0.0-alpha1", true},
		{"~6.0.0", "6.0.9-beta2", true},
		{"~6.0.0", "6.1.0-alpha1", false},
		{">=6.0.0", "6.0.0-alpha1", true},
		{">=6.0.0", "5.9.9", false},
		{"<7.0.0", "6.0.0-alpha1", true},
		{"<7.0.0", "7.0.0-alpha1", false},
		{">6.0.0", "6.0.0-alpha1", false},
		{">6.0.0", "6.0.1", true},
		{"<=7.0.0", "7.0.0-alpha1", true},
		{"6.0.*", "6.0.0-alpha1", true},
		{"6.0.*", "6.1.0-alpha1", false},

		// Explicit stability suffix preserves exact bound.
		{"^6.0.0-alpha1", "6.0.0-alpha1", true},
		{"^6.0.0-alpha1", "6.0.0-beta1", true},
		{"^6.0.0-alpha1", "6.0.0-dev", false},
		{">=6.0.0-beta1", "6.0.0-alpha1", false},
		{">=6.0.0-beta1", "6.0.0-beta1", true},
	}

	for _, tc := range tests {
		t.Run(tc.constraint+"_"+tc.version, func(t *testing.T) {
			c, err := ParseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tc.constraint, err)
			}
			v, err := Parse(tc.version)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.version, err)
			}
			if got := c.Matches(v); got != tc.want {
				t.Errorf("Constraint(%q).Matches(%q) = %v, want %v", tc.constraint, tc.version, got, tc.want)
			}
		})
	}
}

func TestParseConstraintErrors(t *testing.T) {
	tests := []string{
		"",
		"???",
		">=",
		"^",
	}

	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			_, err := ParseConstraint(tc)
			if err == nil {
				t.Errorf("expected error for %q", tc)
			}
		})
	}
}

func TestConstraintString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"^1.2.3", ">=1.2.3-dev <2.0.0-dev"},
		{"~1.2.3", ">=1.2.3-dev <1.3.0-dev"},
		{"1.0.*", ">=1.0.0-dev <1.1.0-dev"},
		{">=1.0.0 <2.0.0", ">=1.0.0-dev <2.0.0-dev"},
		{"^1.0 || ^2.0", ">=1.0.0-dev <2.0.0-dev || >=2.0.0-dev <3.0.0-dev"},
		{"^1.0|^2.0", ">=1.0.0-dev <2.0.0-dev || >=2.0.0-dev <3.0.0-dev"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			c, err := ParseConstraint(tc.input)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tc.input, err)
			}
			if got := c.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConstraintStabilityFlag(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"^2.0.2@RC", ">=2.0.2-dev <3.0.0-dev"},
		{"^1.0@beta", ">=1.0.0-dev <2.0.0-dev"},
		{">=1.0.0@stable", ">=1.0.0-dev"},
		{"~1.2@alpha", ">=1.2.0-dev <2.0.0-dev"},
		{"1.0.0@dev", "1.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			c, err := ParseConstraint(tc.input)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tc.input, err)
			}
			if got := c.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveStability(t *testing.T) {
	tests := []struct {
		constraint       string
		minimumStability Stability
		wantStability    Stability
	}{
		// Explicit @flag overrides minimum-stability.
		{"^1.0@dev", Stable, Dev},
		{"^1.0@alpha", Stable, Alpha},
		{"^1.0@beta", RC, Beta},
		{"^1.0@rc", Stable, RC},
		{"^1.0@stable", Dev, Stable},

		// Dev-branch constraints implicitly require dev stability.
		{"dev-master", Stable, Dev},
		{"dev-main", Stable, Dev},
		{"dev-feature/foo", Stable, Dev},

		// No override uses minimum-stability.
		{"^1.0", Stable, Stable},
		{"^1.0", Beta, Beta},
		{">=1.0.0", Alpha, Alpha},
		{"1.0.0", Dev, Dev},
	}

	for _, tc := range tests {
		t.Run(tc.constraint, func(t *testing.T) {
			c, err := ParseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tc.constraint, err)
			}
			got := c.EffectiveStability(tc.minimumStability)
			if got != tc.wantStability {
				t.Errorf("EffectiveStability(%v) = %v, want %v", tc.minimumStability, got, tc.wantStability)
			}
		})
	}
}

func TestStabilityOverride(t *testing.T) {
	tests := []struct {
		constraint   string
		wantOverride int
	}{
		// No stability flag.
		{"^1.0", NoStabilityOverride},
		{">=1.0.0", NoStabilityOverride},
		{"dev-master", NoStabilityOverride}, // dev-branch is implicit, not explicit

		// Explicit stability flags.
		{"^1.0@dev", int(Dev)},
		{"^1.0@alpha", int(Alpha)},
		{"^1.0@beta", int(Beta)},
		{"^1.0@rc", int(RC)},
		{"^1.0@stable", int(Stable)},
	}

	for _, tc := range tests {
		t.Run(tc.constraint, func(t *testing.T) {
			c, err := ParseConstraint(tc.constraint)
			if err != nil {
				t.Fatalf("ParseConstraint(%q): %v", tc.constraint, err)
			}
			if c.StabilityOverride != tc.wantOverride {
				t.Errorf("StabilityOverride = %v, want %v", c.StabilityOverride, tc.wantOverride)
			}
		})
	}
}
