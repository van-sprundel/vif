package scripts

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/van-sprundel/vif/internal/composer"
)

// Runner executes Composer script events.
type Runner struct {
	projectDir  string
	scripts     composer.Scripts
	autoScripts map[string]string
	w           io.Writer
	visited     map[string]bool // circular reference detection
}

// New creates a script runner for the given project.
func New(projectDir string, scripts composer.Scripts, autoScripts map[string]string, w io.Writer) *Runner {
	return &Runner{
		projectDir:  projectDir,
		scripts:     scripts,
		autoScripts: autoScripts,
		w:           w,
	}
}

// Run executes all handlers registered for the given event.
// Returns nil if no handlers are registered.
func (r *Runner) Run(event string) error {
	handlers := r.scripts[event]
	if len(handlers) == 0 && !r.hasAutoScripts(event) {
		return nil
	}

	fmt.Fprintf(r.w, "Running %s scripts...\n", event)
	r.visited = make(map[string]bool)

	for _, handler := range handlers {
		if err := r.runHandler(handler); err != nil {
			return fmt.Errorf("script %s: %w", event, err)
		}
	}

	// Run Symfony auto-scripts for post-install-cmd and post-update-cmd.
	if r.hasAutoScripts(event) {
		if err := r.runAutoScripts(); err != nil {
			return fmt.Errorf("script %s (auto-scripts): %w", event, err)
		}
	}

	return nil
}

func (r *Runner) hasAutoScripts(event string) bool {
	if len(r.autoScripts) == 0 {
		return false
	}
	return event == "post-install-cmd" || event == "post-update-cmd"
}

func (r *Runner) runHandler(handler string) error {
	switch {
	case strings.HasPrefix(handler, "@putenv "):
		return r.runPutenv(handler[8:])
	case strings.HasPrefix(handler, "@composer "):
		fmt.Fprintf(r.w, "  Skipping @composer command: %s\n", handler)
		return nil
	case strings.HasPrefix(handler, "@php "):
		return r.runShell("php " + handler[5:])
	case strings.HasPrefix(handler, "@"):
		return r.runReference(handler[1:])
	case strings.Contains(handler, "::"):
		return r.runPHPCallback(handler)
	default:
		return r.runShell(handler)
	}
}

func (r *Runner) runPutenv(spec string) error {
	parts := strings.SplitN(spec, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("@putenv: invalid format %q (expected KEY=VALUE)", spec)
	}
	return os.Setenv(parts[0], parts[1])
}

func (r *Runner) runReference(event string) error {
	if r.visited[event] {
		return fmt.Errorf("circular script reference: @%s", event)
	}
	r.visited[event] = true
	defer func() { r.visited[event] = false }()

	handlers := r.scripts[event]
	if len(handlers) == 0 {
		return fmt.Errorf("script event %q referenced by @%s not found", event, event)
	}
	for _, h := range handlers {
		if err := r.runHandler(h); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) runPHPCallback(handler string) error {
	// Execute PHP static method: Vendor\Class::method
	// Bootstrap vendor/autoload.php first.
	code := fmt.Sprintf(`require '%s'; %s();`,
		filepath.Join(r.projectDir, "vendor", "autoload.php"),
		handler,
	)
	return r.runShell(fmt.Sprintf("php -r %q", code))
}

func (r *Runner) runShell(command string) error {
	fmt.Fprintf(r.w, "  > %s\n", command)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = r.projectDir
	cmd.Stdout = r.w
	cmd.Stderr = r.w

	// Set Composer-compatible environment variables.
	cmd.Env = append(os.Environ(),
		"COMPOSER_VENDOR_DIR="+filepath.Join(r.projectDir, "vendor"),
		"COMPOSER_BIN_DIR="+filepath.Join(r.projectDir, "vendor", "bin"),
	)

	return cmd.Run()
}

func (r *Runner) runAutoScripts() error {
	for command, scriptType := range r.autoScripts {
		var shell string
		switch scriptType {
		case "symfony-cmd":
			shell = "php bin/console " + command
		case "php-script":
			shell = "php " + command
		case "script":
			shell = command
		default:
			fmt.Fprintf(r.w, "  Skipping unknown auto-script type %q for %s\n", scriptType, command)
			continue
		}
		if err := r.runShell(shell); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
	}
	return nil
}
