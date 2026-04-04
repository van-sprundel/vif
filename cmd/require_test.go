package cmd

import (
	"testing"
)

func TestRecommendedConstraint(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"2.1.5", "^2.1"},
		{"1.0.0", "^1.0"},
		{"3.0.0.0", "^3.0"},
		{"0.1.0", "*"},
		{"dev-main", "*"},
		{"1.0.0-beta1", "^1.0"},
		{"10.2.3", "^10.2"},
		{"invalid-version", "*"},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got := recommendedConstraint(tt.version)
			if got != tt.want {
				t.Errorf("recommendedConstraint(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}
