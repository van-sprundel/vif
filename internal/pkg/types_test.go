package pkg_test

import (
	"encoding/json"
	"testing"

	"github.com/van-sprundel/vif/internal/pkg"
)

func TestAutoloadUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		check    func(t *testing.T, a pkg.Autoload)
	}{
		{
			name:  "psr-4 string value normalized to slice",
			input: `{"psr-4":{"Foo\\":"src/"}}`,
			check: func(t *testing.T, a pkg.Autoload) {
				t.Helper()
				paths, ok := a.PSR4[`Foo\`]
				if !ok {
					t.Fatal("key Foo\\ not found")
				}
				if len(paths) != 1 || paths[0] != "src/" {
					t.Errorf("paths = %v, want [\"src/\"]", paths)
				}
			},
		},
		{
			name:  "psr-4 array value preserved",
			input: `{"psr-4":{"Bar\\":["src/","lib/"]}}`,
			check: func(t *testing.T, a pkg.Autoload) {
				t.Helper()
				paths, ok := a.PSR4[`Bar\`]
				if !ok {
					t.Fatal("key Bar\\ not found")
				}
				if len(paths) != 2 || paths[0] != "src/" || paths[1] != "lib/" {
					t.Errorf("paths = %v, want [\"src/\",\"lib/\"]", paths)
				}
			},
		},
		{
			name:  "psr-0 string value normalized to slice",
			input: `{"psr-0":{"Legacy_":"lib/"}}`,
			check: func(t *testing.T, a pkg.Autoload) {
				t.Helper()
				paths, ok := a.PSR0["Legacy_"]
				if !ok {
					t.Fatal("key Legacy_ not found")
				}
				if len(paths) != 1 || paths[0] != "lib/" {
					t.Errorf("paths = %v, want [\"lib/\"]", paths)
				}
			},
		},
		{
			name:  "classmap and files populated",
			input: `{"classmap":["src/Foo.php"],"files":["bootstrap.php"]}`,
			check: func(t *testing.T, a pkg.Autoload) {
				t.Helper()
				if len(a.Classmap) != 1 || a.Classmap[0] != "src/Foo.php" {
					t.Errorf("Classmap = %v, want [\"src/Foo.php\"]", a.Classmap)
				}
				if len(a.Files) != 1 || a.Files[0] != "bootstrap.php" {
					t.Errorf("Files = %v, want [\"bootstrap.php\"]", a.Files)
				}
			},
		},
		{
			name:  "empty autoload",
			input: `{}`,
			check: func(t *testing.T, a pkg.Autoload) {
				t.Helper()
				if a.PSR4 != nil {
					t.Errorf("PSR4 = %v, want nil", a.PSR4)
				}
				if a.PSR0 != nil {
					t.Errorf("PSR0 = %v, want nil", a.PSR0)
				}
			},
		},
		{
			name:    "invalid JSON",
			input:   `{invalid`,
			wantErr: true,
		},
		{
			name:    "psr-4 value is invalid type",
			input:   `{"psr-4":{"Foo\\": 42}}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var a pkg.Autoload
			err := json.Unmarshal([]byte(tc.input), &a)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}
