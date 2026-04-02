package composerauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config stores the subset of Composer auth settings vif can apply to HTTP requests.
type Config struct {
	Bearer      map[string]string
	HTTPBasic   map[string]HTTPBasicAuth
	GitLabToken map[string]string
	GitLabOAuth map[string]string
}

// HTTPBasicAuth holds username/password credentials for a host.
type HTTPBasicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type rawConfig struct {
	Bearer      map[string]string        `json:"bearer"`
	HTTPBasic   map[string]HTTPBasicAuth `json:"http-basic"`
	GitLabToken map[string]string        `json:"gitlab-token"`
	GitLabOAuth map[string]string        `json:"gitlab-oauth"`
}

// Load reads Composer auth settings from auth.json locations plus COMPOSER_AUTH.
func Load() (*Config, error) {
	cfg := &Config{}

	for _, path := range authFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("composer auth: read %s: %w", path, err)
		}
		if err := cfg.mergeJSON(data); err != nil {
			return nil, fmt.Errorf("composer auth: parse %s: %w", path, err)
		}
	}

	if raw := strings.TrimSpace(os.Getenv("COMPOSER_AUTH")); raw != "" {
		if err := cfg.mergeJSON([]byte(raw)); err != nil {
			return nil, fmt.Errorf("composer auth: parse COMPOSER_AUTH: %w", err)
		}
	}

	if cfg.empty() {
		return nil, nil
	}
	return cfg, nil
}

func authFileCandidates() []string {
	seen := make(map[string]struct{})
	var paths []string
	add := func(path string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(cwd, "auth.json"))
	}

	if composerHome := strings.TrimSpace(os.Getenv("COMPOSER_HOME")); composerHome != "" {
		add(filepath.Join(composerHome, "auth.json"))
	}

	if xdgConfigHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdgConfigHome != "" {
		add(filepath.Join(xdgConfigHome, "composer", "auth.json"))
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".config", "composer", "auth.json"))
		add(filepath.Join(home, ".composer", "auth.json"))
	}

	return paths
}

func (c *Config) mergeJSON(data []byte) error {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	mergeStringMap(&c.Bearer, raw.Bearer)
	mergeStringMap(&c.GitLabToken, raw.GitLabToken)
	mergeStringMap(&c.GitLabOAuth, raw.GitLabOAuth)
	if len(raw.HTTPBasic) > 0 {
		if c.HTTPBasic == nil {
			c.HTTPBasic = make(map[string]HTTPBasicAuth, len(raw.HTTPBasic))
		}
		for host, creds := range raw.HTTPBasic {
			c.HTTPBasic[normalizeHost(host)] = creds
		}
	}

	return nil
}

func mergeStringMap(dst *map[string]string, src map[string]string) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]string, len(src))
	}
	for host, value := range src {
		(*dst)[normalizeHost(host)] = value
	}
}

// ApplyRequest adds matching Composer auth credentials to req when available.
func (c *Config) ApplyRequest(req *http.Request) {
	if c == nil || req == nil || req.URL == nil {
		return
	}

	host := normalizeHost(req.URL.Host)
	if host == "" {
		return
	}

	if isGitLabArchiveURL(req.URL) {
		if token := c.GitLabToken[host]; token != "" {
			req.Header.Set("Private-Token", token)
			return
		}
		if token := c.GitLabOAuth[host]; token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			return
		}
	}

	if token := c.Bearer[host]; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return
	}

	if creds, ok := c.HTTPBasic[host]; ok {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = strings.ToLower(u.Host)
		}
	}
	if parsed, err := url.Parse("https://" + host); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return host
}

func isGitLabArchiveURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := normalizeHost(u.Host)
	return (host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com")) &&
		strings.Contains(u.Path, "/api/v4/projects/") &&
		strings.Contains(u.Path, "/packages/composer/archives/")
}

func (c *Config) empty() bool {
	return len(c.Bearer) == 0 &&
		len(c.HTTPBasic) == 0 &&
		len(c.GitLabToken) == 0 &&
		len(c.GitLabOAuth) == 0
}
