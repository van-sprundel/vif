package version

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input   string
		want    Version
		wantErr bool
	}{
		// Simple versions.
		{"1.0.0", Version{Major: 1, Minor: 0, Patch: 0, Stability: Stable}, false},
		{"2.3.4", Version{Major: 2, Minor: 3, Patch: 4, Stability: Stable}, false},
		{"0.1.0", Version{Major: 0, Minor: 1, Patch: 0, Stability: Stable}, false},

		// v-prefix.
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3, Stability: Stable}, false},
		{"V2.0.0", Version{Major: 2, Minor: 0, Patch: 0, Stability: Stable}, false},

		// Two-segment (missing patch).
		{"1.0", Version{Major: 1, Minor: 0, Patch: 0, Stability: Stable}, false},
		{"v3.2", Version{Major: 3, Minor: 2, Patch: 0, Stability: Stable}, false},

		// Four-segment (Composer allows it).
		{"1.2.3.4", Version{Major: 1, Minor: 2, Patch: 3, Tweak: 4, Stability: Stable}, false},

		// Stability suffixes.
		{"1.0.0-alpha1", Version{Major: 1, Stability: Alpha, StabilityNum: 1}, false},
		{"1.0.0-beta2", Version{Major: 1, Stability: Beta, StabilityNum: 2}, false},
		{"1.0.0-RC3", Version{Major: 1, Stability: RC, StabilityNum: 3}, false},
		{"1.0.0-rc1", Version{Major: 1, Stability: RC, StabilityNum: 1}, false},
		{"2.0.0-patch1", Version{Major: 2, Stability: Patch, StabilityNum: 1}, false},
		{"2.0.0-p2", Version{Major: 2, Stability: Patch, StabilityNum: 2}, false},
		{"1.0.0-dev", Version{Major: 1, Stability: Dev}, false},

		// Dev branches.
		{"dev-main", Version{Dev: true, DevBranch: "main", Stability: Dev}, false},
		{"dev-master", Version{Dev: true, DevBranch: "master", Stability: Dev}, false},
		{"dev-feature/foo", Version{Dev: true, DevBranch: "feature/foo", Stability: Dev}, false},

		// Errors.
		{"", Version{}, true},
		{"not-a-version", Version{}, true},
		{"dev-", Version{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Parse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}

			// Compare key fields.
			if got.Major != tc.want.Major || got.Minor != tc.want.Minor || got.Patch != tc.want.Patch {
				t.Errorf("Parse(%q) version = %d.%d.%d, want %d.%d.%d",
					tc.input, got.Major, got.Minor, got.Patch, tc.want.Major, tc.want.Minor, tc.want.Patch)
			}
			if got.Tweak != tc.want.Tweak {
				t.Errorf("Parse(%q) tweak = %d, want %d", tc.input, got.Tweak, tc.want.Tweak)
			}
			if got.Stability != tc.want.Stability {
				t.Errorf("Parse(%q) stability = %d, want %d", tc.input, got.Stability, tc.want.Stability)
			}
			if got.StabilityNum != tc.want.StabilityNum {
				t.Errorf("Parse(%q) stabilityNum = %d, want %d", tc.input, got.StabilityNum, tc.want.StabilityNum)
			}
			if got.Dev != tc.want.Dev {
				t.Errorf("Parse(%q) dev = %v, want %v", tc.input, got.Dev, tc.want.Dev)
			}
			if got.DevBranch != tc.want.DevBranch {
				t.Errorf("Parse(%q) devBranch = %q, want %q", tc.input, got.DevBranch, tc.want.DevBranch)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int // -1, 0, 1
	}{
		// Equal.
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},

		// Major ordering.
		{"1.0.0", "2.0.0", -1},
		{"3.0.0", "2.0.0", 1},

		// Minor ordering.
		{"1.1.0", "1.2.0", -1},
		{"1.3.0", "1.2.0", 1},

		// Patch ordering.
		{"1.0.1", "1.0.2", -1},
		{"1.0.3", "1.0.2", 1},

		// Tweak ordering.
		{"1.0.0.1", "1.0.0.2", -1},

		// Stability ordering: dev < alpha < beta < RC < stable < patch.
		{"1.0.0-dev", "1.0.0-alpha1", -1},
		{"1.0.0-alpha1", "1.0.0-beta1", -1},
		{"1.0.0-beta1", "1.0.0-RC1", -1},
		{"1.0.0-RC1", "1.0.0", -1},
		{"1.0.0", "1.0.0-patch1", -1},

		// Same stability, different number.
		{"1.0.0-alpha1", "1.0.0-alpha2", -1},
		{"1.0.0-beta3", "1.0.0-beta1", 1},

		// Pre-release is less than stable.
		{"2.0.0-RC1", "1.0.0", 1}, // 2.x > 1.x even with RC

		// Dev branches compare as very low.
		{"dev-main", "0.0.1", -1},
		{"dev-main", "dev-master", 0}, // dev branches are equal priority
	}

	for _, tc := range tests {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			a, err := Parse(tc.a)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.a, err)
			}
			b, err := Parse(tc.b)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.b, err)
			}

			got := Compare(a, b)
			if got != tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}

			// Verify symmetry.
			reverse := Compare(b, a)
			if reverse != -tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d (reverse)", tc.b, tc.a, reverse, -tc.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1.0.0", "1.0.0"},
		{"v2.3.4", "2.3.4"},
		{"1.0.0-alpha1", "1.0.0-alpha1"},
		{"1.0.0-RC3", "1.0.0-RC3"},
		{"dev-main", "dev-main"},
		{"1.2.3.4", "1.2.3.4"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			v, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if got := v.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStabilityAtLeast(t *testing.T) {
	tests := []struct {
		version  string
		minStab  Stability
		want     bool
	}{
		{"1.0.0", Stable, true},
		{"1.0.0", Dev, true},
		{"1.0.0-RC1", RC, true},
		{"1.0.0-RC1", Stable, false},
		{"1.0.0-beta1", Beta, true},
		{"1.0.0-beta1", RC, false},
		{"1.0.0-alpha1", Alpha, true},
		{"1.0.0-alpha1", Beta, false},
		{"1.0.0-dev", Dev, true},
		{"1.0.0-dev", Alpha, false},
		{"dev-main", Dev, true},
		{"dev-main", Alpha, false},
	}

	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			v, err := Parse(tc.version)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := v.StabilityAtLeast(tc.minStab); got != tc.want {
				t.Errorf("StabilityAtLeast(%d) = %v, want %v", tc.minStab, got, tc.want)
			}
		})
	}
}
