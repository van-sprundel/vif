package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const githubRepo = "van-sprundel/vif"

func newSelfUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "self-update",
		Short:        "Update vif to the latest version",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate()
		},
	}
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func runSelfUpdate() error {
	w := os.Stderr

	current := Version
	fmt.Fprintf(w, "Current version: %s\n", current)

	// Fetch latest release.
	fmt.Fprint(w, "Checking for updates...")
	release, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	fmt.Fprintf(w, " %s\n", release.TagName)

	if current == release.TagName {
		fmt.Fprintln(w, "Already up to date.")
		return nil
	}

	if current != "dev" {
		fmt.Fprintf(w, "Updating %s → %s\n", current, release.TagName)
	}

	// Find matching asset.
	assetName := fmt.Sprintf("vif-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no release binary found for %s/%s (looking for %s)", runtime.GOOS, runtime.GOARCH, assetName)
	}

	// Download new binary.
	fmt.Fprintf(w, "Downloading %s...\n", assetName)
	newBinary, err := downloadBinary(downloadURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer os.Remove(newBinary)

	// Replace current binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}

	// Get permissions of current binary.
	info, err := os.Stat(execPath)
	if err != nil {
		return fmt.Errorf("stat current binary: %w", err)
	}

	// Atomic replace: rename old, rename new, remove old.
	oldPath := execPath + ".old"
	if err := os.Rename(execPath, oldPath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(w, "Permission denied, retrying with sudo...")
			if err := sudoReplace(newBinary, execPath, info.Mode()); err != nil {
				return err
			}
			fmt.Fprintf(w, "Updated to %s\n", release.TagName)
			return nil
		}
		return fmt.Errorf("backup current binary: %w", err)
	}

	if err := os.Rename(newBinary, execPath); err != nil {
		// Rollback.
		_ = os.Rename(oldPath, execPath)
		return fmt.Errorf("replace binary: %w", err)
	}

	if err := os.Chmod(execPath, info.Mode()); err != nil {
		fmt.Fprintf(w, "Warning: could not set permissions: %v\n", err)
	}

	_ = os.Remove(oldPath)

	fmt.Fprintf(w, "Updated to %s\n", release.TagName)
	return nil
}

func sudoReplace(newBinary, execPath string, mode os.FileMode) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("sudo not found, please run as root or install to a user-writable location")
	}

	// Use sudo to copy the new binary into place and set permissions.
	cp := exec.Command(sudo, "cp", newBinary, execPath)
	cp.Stdin = os.Stdin
	cp.Stdout = os.Stderr
	cp.Stderr = os.Stderr
	if err := cp.Run(); err != nil {
		return fmt.Errorf("sudo cp: %w", err)
	}

	chmod := exec.Command(sudo, "chmod", fmt.Sprintf("%o", mode.Perm()), execPath)
	chmod.Stdin = os.Stdin
	chmod.Stdout = os.Stderr
	chmod.Stderr = os.Stderr
	if err := chmod.Run(); err != nil {
		return fmt.Errorf("sudo chmod: %w", err)
	}

	return nil
}

func fetchLatestRelease() (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &release, nil
}

func downloadBinary(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "vif-update-*")
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()

	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}

	return tmp.Name(), nil
}
