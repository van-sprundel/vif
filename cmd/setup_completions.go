package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newSetupCompletionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "setup-completions",
		Short:        "Install shell completions for your current shell",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := detectShell()
			if shell == "" {
				return fmt.Errorf("could not detect shell, set $SHELL and retry")
			}

			root := cmd.Root()

			switch shell {
			case "bash":
				return setupBash(root)
			case "zsh":
				return setupZsh(root)
			case "fish":
				return setupFish(root)
			default:
				return fmt.Errorf("unsupported shell %q — run `vif completion %s` to generate manually", shell, shell)
			}
		},
	}
}

func detectShell() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return ""
	}
	return filepath.Base(sh)
}

func setupBash(root *cobra.Command) error {
	dir := bashCompletionDir()
	if dir == "" {
		return fmt.Errorf("could not find bash completion directory — install bash-completion and retry")
	}

	path := filepath.Join(dir, "vif")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write %s: %w (try running with sudo)", path, err)
	}
	defer f.Close()

	if err := root.GenBashCompletionV2(f, true); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Installed bash completions to %s\n", path)
	fmt.Fprintln(os.Stderr, "Restart your shell or run: source "+path)
	return nil
}

func bashCompletionDir() string {
	// User-local dir first.
	if home, err := os.UserHomeDir(); err == nil {
		local := filepath.Join(home, ".local", "share", "bash-completion", "completions")
		if dirExists(local) {
			return local
		}
	}

	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		local := filepath.Join(xdg, "bash-completion", "completions")
		if dirExists(local) {
			return local
		}
	}

	// System dirs.
	for _, d := range []string{
		"/usr/share/bash-completion/completions",
		"/etc/bash_completion.d",
	} {
		if dirExists(d) {
			return d
		}
	}
	return ""
}

func setupZsh(root *cobra.Command) error {
	// Find a writable dir in $fpath. Prefer user-local dirs.
	dir := zshCompletionDir()
	if dir == "" {
		return fmt.Errorf("no writable zsh completions directory found in $fpath — add a directory to $fpath and retry")
	}

	path := filepath.Join(dir, "_vif")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()

	if err := root.GenZshCompletion(f); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Installed zsh completions to %s\n", path)
	fmt.Fprintln(os.Stderr, "Restart your shell or run: autoload -Uz compinit && compinit")
	return nil
}

func zshCompletionDir() string {
	// Query zsh for fpath.
	out, err := exec.Command("zsh", "-c", "echo $fpath").Output()
	if err != nil {
		return zshFallbackDir()
	}
	dirs := strings.Fields(strings.TrimSpace(string(out)))

	home, _ := os.UserHomeDir()

	// Prefer user-local directories.
	for _, d := range dirs {
		if home != "" && strings.HasPrefix(d, home) && dirWritable(d) {
			return d
		}
	}
	// Fall back to any writable dir.
	for _, d := range dirs {
		if dirWritable(d) {
			return d
		}
	}
	return zshFallbackDir()
}

func zshFallbackDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".zsh", "completions")
	os.MkdirAll(dir, 0o755)
	return dir
}

func setupFish(root *cobra.Command) error {
	dir := fishCompletionDir()
	os.MkdirAll(dir, 0o755)

	path := filepath.Join(dir, "vif.fish")
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close()

	if err := root.GenFishCompletion(f, true); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Installed fish completions to %s\n", path)
	fmt.Fprintln(os.Stderr, "Completions will be available in new shell sessions.")
	return nil
}

func fishCompletionDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "fish", "completions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "fish", "completions")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dirWritable(path string) bool {
	if !dirExists(path) {
		return false
	}
	tmp := filepath.Join(path, ".vif-write-test")
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(tmp)
	return true
}
