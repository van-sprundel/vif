package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const issueRepo = "van-sprundel/vif"

func newReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "report",
		Short:        "Open a GitHub issue pre-filled with your composer.json and composer.lock for bug reproduction",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReport()
		},
	}
}

func runReport() error {
	w := os.Stderr

	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("the `gh` CLI is required — install it from https://cli.github.com")
	}

	composerJSON, err := os.ReadFile("composer.json")
	if err != nil {
		return fmt.Errorf("could not read composer.json: %w (run this from your project root)", err)
	}

	composerLock, lockErr := os.ReadFile("composer.lock")
	hasLock := lockErr == nil

	fmt.Fprintln(w, "This will upload a gist and open a GitHub issue draft on "+issueRepo+" containing:")
	fmt.Fprintln(w, "  - composer.json")
	if hasLock {
		fmt.Fprintln(w, "  - composer.lock")
	}
	fmt.Fprintln(w, "  - vif version, OS, and arch")
	fmt.Fprintln(w)
	fmt.Fprint(w, "Continue? [y/N] ")

	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(w, "Aborted.")
		return nil
	}

	gistURL, err := createGist(composerJSON, composerLock, hasLock)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Uploaded files to gist: %s\n", gistURL)

	body := buildReportBody(gistURL)

	// Write body to a temp file and use --body-file to avoid arg length limits,
	// then --web to open the browser so the user can review before submitting.
	tmp, err := os.CreateTemp("", "vif-report-body-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	gh := exec.Command("gh", "issue", "create",
		"--repo", issueRepo,
		"--title", "Bug report via `vif report`",
		"--body-file", tmp.Name(),
		"--web",
	)
	gh.Stdin = os.Stdin
	gh.Stdout = os.Stderr
	gh.Stderr = os.Stderr

	if err := gh.Run(); err != nil {
		return fmt.Errorf("gh issue create failed: %w", err)
	}

	fmt.Fprintln(w, "Opened issue draft in your browser — add a description and submit when ready.")
	return nil
}

func createGist(composerJSON, composerLock []byte, hasLock bool) (string, error) {
	args := []string{"gist", "create", "--public", "-d", "vif bug report files"}

	jsonTmp, err := writeTempFile("composer.json", composerJSON)
	if err != nil {
		return "", err
	}
	defer os.Remove(jsonTmp)
	defer os.Remove(filepath.Dir(jsonTmp))
	args = append(args, jsonTmp)

	if hasLock {
		lockTmp, err := writeTempFile("composer.lock", composerLock)
		if err != nil {
			return "", err
		}
		defer os.Remove(lockTmp)
		defer os.Remove(filepath.Dir(lockTmp))
		args = append(args, lockTmp)
	}

	var out bytes.Buffer
	gh := exec.Command("gh", args...)
	gh.Stdout = &out
	gh.Stderr = os.Stderr

	if err := gh.Run(); err != nil {
		return "", fmt.Errorf("gh gist create failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

func writeTempFile(name string, data []byte) (string, error) {
	dir, err := os.MkdirTemp("", "vif-report-*")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func buildReportBody(gistURL string) string {
	var b strings.Builder

	b.WriteString("## Environment\n\n")
	b.WriteString(fmt.Sprintf("- **vif version:** %s\n", Version))
	b.WriteString(fmt.Sprintf("- **OS:** %s/%s\n", runtime.GOOS, runtime.GOARCH))
	b.WriteString(fmt.Sprintf("\n## Files\n\n"))
	b.WriteString(fmt.Sprintf("composer.json and composer.lock: %s\n", gistURL))
	b.WriteString(fmt.Sprintf("\n## Description\n\n"))
	b.WriteString("_Please describe the issue you encountered:_\n\n")

	return b.String()
}
