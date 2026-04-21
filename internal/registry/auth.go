package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials holds authentication material for one registry.
type Credentials struct {
	Username      string
	Password      string
	IdentityToken string // OAuth refresh token (mostly used by Docker Hub credential helper)
}

// Empty reports whether no credentials are set.
func (c Credentials) Empty() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == ""
}

// AuthConfig maps registry hosts to credentials.
type AuthConfig struct {
	creds map[string]Credentials
}

func NewAuthConfig() *AuthConfig {
	return &AuthConfig{creds: map[string]Credentials{}}
}

// Set stores credentials for a registry. host may carry a scheme/path prefix
// and will be normalized.
func (a *AuthConfig) Set(host string, c Credentials) {
	if a.creds == nil {
		a.creds = map[string]Credentials{}
	}
	a.creds[normalizeHost(host)] = c
}

// For looks up credentials for a registry, accepting common Docker Hub aliases.
func (a *AuthConfig) For(host string) (Credentials, bool) {
	if a == nil || len(a.creds) == 0 {
		return Credentials{}, false
	}
	key := normalizeHost(host)
	if c, ok := a.creds[key]; ok {
		return c, true
	}
	if key == "registry-1.docker.io" || key == "index.docker.io" || key == "docker.io" {
		for _, alt := range []string{"registry-1.docker.io", "index.docker.io", "docker.io"} {
			if c, ok := a.creds[alt]; ok {
				return c, true
			}
		}
	}
	return Credentials{}, false
}

func normalizeHost(s string) string {
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

// LoadDockerConfig parses the "auths" section of ~/.docker/config.json.
// If path is empty, it uses $DOCKER_CONFIG/config.json or ~/.docker/config.json.
// A missing file returns an empty AuthConfig rather than an error.
func LoadDockerConfig(path string) (*AuthConfig, error) {
	if path == "" {
		if env := os.Getenv("DOCKER_CONFIG"); env != "" {
			path = filepath.Join(env, "config.json")
		} else if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".docker", "config.json")
		}
	}
	if path == "" {
		return NewAuthConfig(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewAuthConfig(), nil
		}
		return nil, err
	}
	var raw struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	ac := NewAuthConfig()
	for host, e := range raw.Auths {
		c := Credentials{
			Username:      e.Username,
			Password:      e.Password,
			IdentityToken: e.IdentityToken,
		}
		if e.Auth != "" {
			if decoded, err := base64.StdEncoding.DecodeString(e.Auth); err == nil {
				if i := strings.Index(string(decoded), ":"); i >= 0 {
					c.Username = string(decoded[:i])
					c.Password = string(decoded[i+1:])
				}
			}
		}
		if !c.Empty() {
			ac.Set(host, c)
		}
	}
	return ac, nil
}
