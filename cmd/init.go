package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

var packageNameRegex = regexp.MustCompile(`^[a-z0-9]([_.\-]?[a-z0-9]+)*/[a-z0-9](([_.]|\-{1,2})?[a-z0-9]+)*$`)

// newInitCmd returns the `vif init` command.
func newInitCmd() *cobra.Command {
	var (
		name         string
		description  string
		author       string
		pkgType      string
		license      string
		stability    string
		autoloadPSR4 string
	)

	cmd := &cobra.Command{
		Use:          "init",
		Short:        "Create a new composer.json in the current directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(name, description, author, pkgType, license, stability, autoloadPSR4)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "package name (vendor/name)")
	cmd.Flags().StringVar(&description, "description", "", "package description")
	cmd.Flags().StringVar(&author, "author", "", `author ("Name <email>")`)
	cmd.Flags().StringVar(&pkgType, "type", "library", "package type")
	cmd.Flags().StringVarP(&license, "license", "l", "", "SPDX license identifier")
	cmd.Flags().StringVarP(&stability, "stability", "s", "", "minimum stability")
	cmd.Flags().StringVarP(&autoloadPSR4, "autoload", "a", "", "PSR-4 autoload mapping directory (e.g. src/)")

	return cmd
}

func runInit(name, description, author, pkgType, license, stability, autoloadDir string) error {
	composerPath := "composer.json"
	if _, err := os.Stat(composerPath); err == nil {
		return fmt.Errorf("composer.json already exists in the current directory")
	}

	reader := bufio.NewReader(os.Stdin)
	w := os.Stderr

	// Name.
	if name == "" {
		defaultName := guessPackageName()
		if defaultName != "" {
			fmt.Fprintf(w, "Package name (<vendor>/<name>) [%s]: ", defaultName)
		} else {
			fmt.Fprint(w, "Package name (<vendor>/<name>): ")
		}
		input := readLine(reader)
		if input == "" {
			name = defaultName
		} else {
			name = input
		}
	}
	if name != "" && !packageNameRegex.MatchString(name) {
		return fmt.Errorf("invalid package name %q (must match vendor/name format with lowercase alphanumeric characters)", name)
	}

	// Description.
	if description == "" {
		fmt.Fprint(w, "Description []: ")
		description = readLine(reader)
	}

	// Author.
	if author == "" {
		defaultAuthor := guessAuthor()
		if defaultAuthor != "" {
			fmt.Fprintf(w, "Author [%s]: ", defaultAuthor)
		} else {
			fmt.Fprint(w, "Author (Name <email>): ")
		}
		input := readLine(reader)
		if input == "" {
			author = defaultAuthor
		} else {
			author = input
		}
	}

	// License.
	if license == "" {
		fmt.Fprint(w, "License []: ")
		license = readLine(reader)
	}

	// Minimum stability.
	if stability == "" {
		fmt.Fprint(w, "Minimum stability []: ")
		stability = readLine(reader)
	}

	// Build composer.json.
	cjMap := make(map[string]interface{})
	if name != "" {
		cjMap["name"] = name
	}
	if description != "" {
		cjMap["description"] = description
	}
	if pkgType != "" {
		cjMap["type"] = pkgType
	}
	if license != "" {
		cjMap["license"] = license
	}
	if author != "" {
		authors := []map[string]string{parseAuthorString(author)}
		cjMap["authors"] = authors
	}
	if stability != "" {
		cjMap["minimum-stability"] = stability
	}

	cjMap["require"] = map[string]string{}

	// Autoload.
	if autoloadDir != "" {
		ns := namespaceFromPackageName(name)
		cjMap["autoload"] = map[string]interface{}{
			"psr-4": map[string]string{
				ns + `\`: autoloadDir,
			},
		}
	}

	data, err := json.MarshalIndent(cjMap, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to encode composer.json: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(composerPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write composer.json: %w", err)
	}

	fmt.Fprintf(w, "Created %s\n", composerPath)

	// Create autoload directory if specified.
	if autoloadDir != "" {
		if err := os.MkdirAll(autoloadDir, 0o755); err != nil {
			return fmt.Errorf("failed to create autoload directory: %w", err)
		}
	}

	// Offer to add vendor/ to .gitignore.
	if _, err := os.Stat(".git"); err == nil {
		gitignorePath := ".gitignore"
		if !gitignoreContains(gitignorePath, "/vendor/") {
			fmt.Fprint(w, "Add /vendor/ to .gitignore? [Y/n]: ")
			input := readLine(reader)
			if input == "" || strings.ToLower(input) == "y" || strings.ToLower(input) == "yes" {
				f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					fmt.Fprintln(f, "/vendor/")
					f.Close()
				}
			}
		}
	}

	return nil
}

func readLine(reader *bufio.Reader) string {
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func guessPackageName() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	base := filepath.Base(dir)

	// Try git config for vendor part.
	vendor := gitConfig("github.user")
	if vendor == "" {
		vendor = gitConfig("user.name")
	}
	if vendor == "" {
		return ""
	}
	vendor = strings.ToLower(strings.ReplaceAll(vendor, " ", "-"))
	base = strings.ToLower(base)
	return vendor + "/" + base
}

func guessAuthor() string {
	name := gitConfig("user.name")
	email := gitConfig("user.email")
	if name == "" {
		return ""
	}
	if email != "" {
		return fmt.Sprintf("%s <%s>", name, email)
	}
	return name
}

func gitConfig(key string) string {
	out, err := exec.Command("git", "config", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseAuthorString(s string) map[string]string {
	result := make(map[string]string)
	// Parse "Name <email>" format.
	if idx := strings.Index(s, "<"); idx > 0 {
		result["name"] = strings.TrimSpace(s[:idx])
		if end := strings.Index(s, ">"); end > idx {
			result["email"] = s[idx+1 : end]
		}
	} else {
		result["name"] = strings.TrimSpace(s)
	}
	return result
}

// namespaceFromPackageName converts "vendor/package-name" to "Vendor\PackageName".
func namespaceFromPackageName(name string) string {
	if name == "" {
		return ""
	}
	parts := strings.SplitN(name, "/", 2)
	var ns []string
	for _, part := range parts {
		// Replace non-alphanumeric with spaces, then title-case, then strip spaces.
		var b strings.Builder
		for i, r := range part {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if i == 0 || (i > 0 && !unicode.IsLetter(rune(part[i-1])) && !unicode.IsDigit(rune(part[i-1]))) {
					b.WriteRune(unicode.ToUpper(r))
				} else {
					b.WriteRune(r)
				}
			}
		}
		ns = append(ns, b.String())
	}
	return strings.Join(ns, `\`)
}

func gitignoreContains(path, entry string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return true
		}
	}
	return false
}
