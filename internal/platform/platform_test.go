package platform

import (
	"testing"

	"github.com/van-sprundel/vif/internal/pkg"
)

func TestVerifyPackages(t *testing.T) {
	pf := &Platform{
		PHPVersion: "8.3.12",
		Extensions: map[string]bool{
			"ext-curl":     true,
			"ext-mbstring": true,
			"ext-json":     true,
		},
	}

	tests := []struct {
		name     string
		packages []pkg.Package
		wantErrs int
	}{
		{
			name: "all satisfied",
			packages: []pkg.Package{
				{
					Name: "acme/http",
					Require: map[string]string{
						"php":       "^8.1",
						"ext-curl":  "*",
						"acme/core": "^1.0",
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "php version too low",
			packages: []pkg.Package{
				{
					Name: "acme/http",
					Require: map[string]string{
						"php": ">=9.0",
					},
				},
			},
			wantErrs: 1,
		},
		{
			name: "missing extension",
			packages: []pkg.Package{
				{
					Name: "acme/http",
					Require: map[string]string{
						"ext-redis": "*",
					},
				},
			},
			wantErrs: 1,
		},
		{
			name: "multiple failures",
			packages: []pkg.Package{
				{
					Name: "acme/a",
					Require: map[string]string{
						"php":     ">=9.0",
						"ext-pdo": "*",
					},
				},
				{
					Name: "acme/b",
					Require: map[string]string{
						"ext-redis": "*",
					},
				},
			},
			wantErrs: 3,
		},
		{
			name: "no require map",
			packages: []pkg.Package{
				{Name: "acme/c"},
			},
			wantErrs: 0,
		},
		{
			name: "non-platform requirements ignored",
			packages: []pkg.Package{
				{
					Name: "acme/d",
					Require: map[string]string{
						"acme/other": "^1.0",
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "php-64bit always passes",
			packages: []pkg.Package{
				{
					Name: "acme/e",
					Require: map[string]string{
						"php-64bit": "*",
					},
				},
			},
			wantErrs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := VerifyPackages(pf, tt.packages)
			if len(errs) != tt.wantErrs {
				t.Errorf("VerifyPackages() returned %d errors, want %d: %v", len(errs), tt.wantErrs, errs)
			}
		})
	}
}

func TestVerifyPackagesConstraintMismatch(t *testing.T) {
	pf := &Platform{
		PHPVersion: "8.3.12",
		Extensions: map[string]bool{},
	}

	errs := VerifyPackages(pf, []pkg.Package{
		{
			Name: "pkg/x",
			Require: map[string]string{
				"php": ">=8.4",
			},
		},
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	re := errs[0].(RequirementError)
	if re.PackageName != "pkg/x" {
		t.Errorf("error package = %q, want %q", re.PackageName, "pkg/x")
	}
	if re.Requirement != "php" {
		t.Errorf("error requirement = %q, want %q", re.Requirement, "php")
	}
}
