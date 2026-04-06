package version

import "testing"

func mustParseVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func mustParseConstraint(t *testing.T, s string) Constraint {
	t.Helper()
	c, err := ParseConstraint(s)
	if err != nil {
		t.Fatalf("ParseConstraint(%q): %v", s, err)
	}
	return c
}

func TestConstraintVersionSetContains(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		contains   []string
		excludes   []string
	}{
		{
			name:       "caret",
			constraint: "^1.2.3",
			contains:   []string{"1.2.3", "1.9.9"},
			excludes:   []string{"1.2.2", "2.0.0"},
		},
		{
			name:       "or-groups",
			constraint: "1.0.0 || 2.0.0",
			contains:   []string{"1.0.0", "2.0.0"},
			excludes:   []string{"1.0.1", "3.0.0"},
		},
		{
			name:       "exact-dev-branch",
			constraint: "dev-main",
			contains:   []string{"dev-main"},
			excludes:   []string{"dev-master", "1.0.0"},
		},
		{
			name:       "not-equal",
			constraint: "!=1.0.0",
			contains:   []string{"1.0.1", "dev-main"},
			excludes:   []string{"1.0.0"},
		},
		{
			name:       "gte-excludes-dev-branches",
			constraint: ">=1.0.0",
			contains:   []string{"1.0.0", "2.0.0"},
			excludes:   []string{"dev-main", "dev-master"},
		},
		{
			name:       "lt-excludes-dev-branches",
			constraint: "<2.0.0",
			contains:   []string{"1.0.0"},
			excludes:   []string{"dev-main", "dev-master"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := ConstraintVersionSet(mustParseConstraint(t, tc.constraint))
			for _, raw := range tc.contains {
				if !set.Contains(mustParseVersion(t, raw)) {
					t.Fatalf("expected %q to be in %q", raw, tc.constraint)
				}
			}
			for _, raw := range tc.excludes {
				if set.Contains(mustParseVersion(t, raw)) {
					t.Fatalf("expected %q to be excluded by %q", raw, tc.constraint)
				}
			}
		})
	}
}

func TestVersionSetOperations(t *testing.T) {
	main := Singleton(mustParseVersion(t, "dev-main"))
	master := Singleton(mustParseVersion(t, "dev-master"))
	v100 := Singleton(mustParseVersion(t, "1.0.0"))
	v101 := Singleton(mustParseVersion(t, "1.0.1"))

	if !main.Union(master).Contains(mustParseVersion(t, "dev-main")) {
		t.Fatal("union should include dev-main")
	}
	if !main.Union(master).Contains(mustParseVersion(t, "dev-master")) {
		t.Fatal("union should include dev-master")
	}
	if !main.Intersect(master).IsEmpty() {
		t.Fatal("dev-main and dev-master should have empty intersection")
	}

	unionNumeric := v100.Union(v101)
	if !unionNumeric.Contains(mustParseVersion(t, "1.0.0")) || !unionNumeric.Contains(mustParseVersion(t, "1.0.1")) {
		t.Fatal("numeric union should include both versions")
	}
	if !v100.Intersect(v101).IsEmpty() {
		t.Fatal("different singleton numeric sets should not intersect")
	}

	comp := main.Complement()
	if comp.Contains(mustParseVersion(t, "dev-main")) {
		t.Fatal("complement should exclude dev-main")
	}
	if !comp.Contains(mustParseVersion(t, "dev-master")) {
		t.Fatal("complement should include other dev branches")
	}
	if !comp.Contains(mustParseVersion(t, "1.0.0")) {
		t.Fatal("complement should include numeric versions")
	}
}

func TestConstraintExportGroups(t *testing.T) {
	c := mustParseConstraint(t, "^1.2.3 || >=2.0.0 <3.0.0")
	groups := c.ExportGroups()
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if len(groups[0].Bounds) != 2 || len(groups[1].Bounds) != 2 {
		t.Fatalf("expected 2 bounds per group, got %d and %d", len(groups[0].Bounds), len(groups[1].Bounds))
	}
	if groups[0].Bounds[0].Op != OpGte || groups[0].Bounds[1].Op != OpLt {
		t.Fatalf("unexpected operators in first group: %#v", groups[0].Bounds)
	}
	if groups[1].Bounds[0].Op != OpGte || groups[1].Bounds[1].Op != OpLt {
		t.Fatalf("unexpected operators in second group: %#v", groups[1].Bounds)
	}
}
