package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var backendNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,47}$`)

type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

type ServerConfig struct {
	Name    string            `json:"name,omitempty"`
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	CWD     string            `json:"cwd,omitempty"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg.MCPServers == nil {
		return Config{}, fmt.Errorf("config must contain an mcpServers object")
	}

	for name, server := range cfg.MCPServers {
		if !backendNamePattern.MatchString(name) {
			return Config{}, fmt.Errorf("invalid backend name %q: use 1-48 letters, digits, hyphens or underscores; start with a letter", name)
		}
		if err := server.Validate(name); err != nil {
			return Config{}, err
		}
		cfg.MCPServers[name] = server.ExpandEnvironment()
	}
	return cfg, nil
}

func (s ServerConfig) Validate(name string) error {
	kind := strings.ToLower(strings.TrimSpace(s.Type))
	if kind == "" {
		if s.URL != "" {
			parsed, err := url.Parse(s.URL)
			if err != nil {
				return fmt.Errorf("backend %q has invalid URL: %w", name, err)
			}
			if strings.Contains(parsed.Path, "/sse") {
				kind = "sse"
			} else {
				kind = "streamable-http"
			}
		} else {
			kind = "stdio"
		}
	}

	switch kind {
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("backend %q: command is required for stdio", name)
		}
		if s.URL != "" {
			return fmt.Errorf("backend %q: configure either command or url, not both", name)
		}
	case "sse", "streamable-http":
		if strings.TrimSpace(s.URL) == "" {
			return fmt.Errorf("backend %q: url is required for %s", name, kind)
		}
		parsed, err := url.ParseRequestURI(s.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("backend %q: url must be an absolute http or https URL", name)
		}
		if s.Command != "" {
			return fmt.Errorf("backend %q: configure either command or url, not both", name)
		}
	default:
		return fmt.Errorf("backend %q: unsupported type %q (supported: stdio, sse, streamable-http)", name, kind)
	}
	return nil
}

func (s ServerConfig) BackendType() string {
	if s.Type != "" {
		return strings.ToLower(strings.TrimSpace(s.Type))
	}
	if s.URL == "" {
		return "stdio"
	}
	parsed, err := url.Parse(s.URL)
	if err == nil && strings.Contains(parsed.Path, "/sse") {
		return "sse"
	}
	return "streamable-http"
}

func (s ServerConfig) ExpandEnvironment() ServerConfig {
	s.Command = os.ExpandEnv(s.Command)
	s.URL = os.ExpandEnv(s.URL)
	s.CWD = os.ExpandEnv(s.CWD)
	for i := range s.Args {
		s.Args[i] = os.ExpandEnv(s.Args[i])
	}
	for key, value := range s.Env {
		s.Env[key] = os.ExpandEnv(value)
	}
	for key, value := range s.Headers {
		s.Headers[key] = os.ExpandEnv(value)
	}
	return s
}
