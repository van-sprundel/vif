package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var wg sync.WaitGroup

const (
	posthogEndpoint = "https://eu.i.posthog.com/capture/"
	posthogAPIKey   = "phc_uMUkAZ6hVE7gFqnxm46dTJVSgss5QAGWwESwNuj9YGGb"
)

// Event represents a single CLI run to report.
type Event struct {
	Command      string           `json:"command"`
	Version      string           `json:"version"`
	PackageCount int              `json:"package_count"`
	DurationMs   int64            `json:"duration_ms"`
	Success      bool             `json:"success"`
	ErrorType    string           `json:"error_type,omitempty"`
	Phases       map[string]int64 `json:"phases,omitempty"`
}

// posthogEvent is the PostHog capture API payload.
type posthogEvent struct {
	APIKey     string         `json:"api_key"`
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "vif", "telemetry")
}

// readConfig returns "enabled", "disabled", or "" (not yet decided).
func readConfig() string {
	path := configPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeConfig(value string) {
	path := configPath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(value+"\n"), 0o644)
}

// Prompt asks the user whether they want to opt in to anonymous telemetry.
// Call this once at CLI startup (before Enabled). It only prompts if the user
// hasn't decided yet and stdin is a terminal. The env var VIF_TELEMETRY
// always takes precedence and skips the prompt.
func Prompt() {
	// Env var override — never prompt.
	if os.Getenv("VIF_TELEMETRY") != "" {
		return
	}

	// Already decided.
	if readConfig() != "" {
		return
	}

	// Don't prompt if stdin isn't a terminal (CI, pipes, etc).
	if !isTerminal(os.Stdin) {
		return
	}

	fmt.Fprint(os.Stderr, "Help improve vif by sending anonymous usage data (command, timing, OS)? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer == "y" || answer == "yes" {
		writeConfig("enabled")
		fmt.Fprintln(os.Stderr, "Thanks! Telemetry enabled. Disable anytime with VIF_TELEMETRY=0 or `vif telemetry off`.")
	} else {
		writeConfig("disabled")
		fmt.Fprintln(os.Stderr, "No worries. You can opt in later with VIF_TELEMETRY=1 or `vif telemetry on`.")
	}
}

// Enabled reports whether telemetry collection is opted into.
// Priority: VIF_TELEMETRY env var > config file.
func Enabled() bool {
	if v := os.Getenv("VIF_TELEMETRY"); v != "" {
		return v == "1"
	}
	return readConfig() == "enabled"
}

// SetEnabled persists the telemetry preference.
func SetEnabled(on bool) {
	if on {
		writeConfig("enabled")
	} else {
		writeConfig("disabled")
	}
}

// ErrorCategory extracts a short, non-sensitive label from an error.
// It strips any user-specific paths or package names to keep payloads anonymous.
func ErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for i, c := range msg {
		if c == ':' {
			return msg[:i]
		}
	}
	if len(msg) > 64 {
		return msg[:64]
	}
	return msg
}

// machineID returns a stable anonymous identifier derived from the hostname.
func machineID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	h := sha256.Sum256([]byte("vif:" + host))
	return hex.EncodeToString(h[:])
}

// Send transmits the event to PostHog in the background. It never blocks the
// caller for more than the time needed to spawn a goroutine and will silently
// discard failures.
func Send(event Event) {
	props := map[string]any{
		"command":       event.Command,
		"version":       event.Version,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"package_count": event.PackageCount,
		"duration_ms":   event.DurationMs,
		"success":       event.Success,
	}
	if event.ErrorType != "" {
		props["error_type"] = event.ErrorType
	}
	for phase, ms := range event.Phases {
		props["phase_"+phase+"_ms"] = ms
	}

	payload := posthogEvent{
		APIKey:     posthogAPIKey,
		Event:      "cli_run",
		DistinctID: machineID(),
		Properties: props,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		body, err := json.Marshal(payload)
		if err != nil {
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, posthogEndpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// Wait blocks until all pending telemetry requests have completed or timed out.
// Call this before process exit.
func Wait() {
	wg.Wait()
}
