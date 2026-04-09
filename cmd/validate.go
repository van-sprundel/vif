package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/van-sprundel/vif/internal/composer"
	"github.com/van-sprundel/vif/internal/lockfile"
)

// newValidateCmd returns the `vif validate` command.
func newValidateCmd() *cobra.Command {
	var (
		noCheckLock bool
		strict      bool
	)

	cmd := &cobra.Command{
		Use:          "validate [file]",
		Short:        "Validate composer.json and composer.lock",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "composer.json"
			if len(args) > 0 {
				path = args[0]
			}
			return runValidate(path, noCheckLock, strict)
		},
	}

	cmd.Flags().BoolVar(&noCheckLock, "no-check-lock", false, "skip lock file freshness check")
	cmd.Flags().BoolVar(&strict, "strict", false, "return non-zero exit code for warnings")

	return cmd
}

func runValidate(path string, noCheckLock, strict bool) error {
	w := os.Stderr
	var errors []string
	var warnings []string

	// Check file exists and is readable.
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(w, "  %s not found.\n", path)
		os.Exit(3)
	}

	// Check valid JSON.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		errors = append(errors, fmt.Sprintf("composer.json is not valid JSON: %v", err))
		printValidationResults(w, errors, warnings)
		os.Exit(2)
	}

	// Check required fields.
	if _, ok := raw["name"]; !ok {
		warnings = append(warnings, `"name" property is missing`)
	}
	if _, ok := raw["description"]; !ok {
		warnings = append(warnings, `"description" property is missing`)
	}

	// Validate the name format.
	if nameRaw, ok := raw["name"]; ok {
		var name string
		if err := json.Unmarshal(nameRaw, &name); err == nil {
			if !packageNameRegex.MatchString(name) {
				errors = append(errors, fmt.Sprintf(`"name" property is invalid: %q`, name))
			}
		}
	}

	// Validate require constraints are parseable.
	validateRequireSection := func(key string) {
		if reqRaw, ok := raw[key]; ok {
			var reqs map[string]string
			if err := json.Unmarshal(reqRaw, &reqs); err != nil {
				errors = append(errors, fmt.Sprintf("%s is not a valid JSON object: %v", key, err))
			} else {
				for pkg, constraint := range reqs {
					if constraint == "" {
						errors = append(errors, fmt.Sprintf(`%s.%s has an empty constraint`, key, pkg))
					}
				}
			}
		}
	}
	validateRequireSection("require")
	validateRequireSection("require-dev")

	// Validate license.
	if licenseRaw, ok := raw["license"]; ok {
		var license string
		if err := json.Unmarshal(licenseRaw, &license); err != nil {
			// Could be an array, which is also valid.
			var licenses []string
			if err := json.Unmarshal(licenseRaw, &licenses); err != nil {
				warnings = append(warnings, "license is not a valid string or array of strings")
			}
		} else if license == "" {
			warnings = append(warnings, `"license" property is empty`)
		}
	} else {
		warnings = append(warnings, `"license" property is missing, it is recommended to have one`)
	}

	// Check lockfile freshness.
	if !noCheckLock {
		if _, err := os.Stat("composer.lock"); err == nil {
			cj, parseErr := composer.Parse(path)
			if parseErr == nil {
				lf, lockErr := lockfile.Parse("composer.lock")
				if lockErr == nil {
					if !lf.IsFresh(cj) {
						warnings = append(warnings, "The lock file is not up to date with the latest changes in composer.json, run `vif update` to update it.")
					}
				}
			}
		}
	}

	// Print results.
	printValidationResults(w, errors, warnings)

	if len(errors) > 0 {
		os.Exit(2)
	}
	if strict && len(warnings) > 0 {
		os.Exit(1)
	}

	return nil
}

func printValidationResults(w *os.File, errors, warnings []string) {
	if len(errors) == 0 && len(warnings) == 0 {
		fmt.Fprintln(w, "./composer.json is valid")
		return
	}

	if len(errors) > 0 {
		fmt.Fprintln(w, "Errors:")
		for _, e := range errors {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	if len(warnings) > 0 {
		if len(errors) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}

	parts := make([]string, 0, 2)
	if len(errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d error(s)", len(errors)))
	}
	if len(warnings) > 0 {
		parts = append(parts, fmt.Sprintf("%d warning(s)", len(warnings)))
	}
	fmt.Fprintf(w, "\n./composer.json is valid for use but has %s\n", strings.Join(parts, " and "))
}
