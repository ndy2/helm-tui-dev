package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type ReleaseProfile struct {
	Namespace    string `yaml:"namespace"`
	ReleaseName  string `yaml:"releaseName"`
	Chart        string `yaml:"chart"`
	Version      string `yaml:"version"`
	RemoteValues string `yaml:"remoteValues"`
	LastSelected int64  `yaml:"lastSelected"` // Unix timestamp for sorting
}

type Config struct {
	Contexts map[string][]ReleaseProfile `yaml:"contexts"`
	// UIHeight is the number of release rows to show (see the --height
	// flag). Persisted so an explicit --height becomes the default for
	// future runs; 0 means "follow the terminal size".
	UIHeight int `yaml:"uiHeight,omitempty"`
}

func GetConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".helm-tui.yaml"
	}
	return filepath.Join(home, ".helm-tui.yaml")
}

func LoadConfig() (*Config, error) {
	path := GetConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Contexts: make(map[string][]ReleaseProfile)}, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	if cfg.Contexts == nil {
		cfg.Contexts = make(map[string][]ReleaseProfile)
	}

	return &cfg, nil
}

func (c *Config) Save() error {
	path := GetConfigPath()
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal yaml: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func (c *Config) AddRelease(context string, p ReleaseProfile) {
	if c.Contexts == nil {
		c.Contexts = make(map[string][]ReleaseProfile)
	}
	c.Contexts[context] = append(c.Contexts[context], p)
}

func (c *Config) RemoveRelease(context string, index int) error {
	releases, ok := c.Contexts[context]
	if !ok {
		return fmt.Errorf("context not found")
	}
	if index < 0 || index >= len(releases) {
		return fmt.Errorf("index out of bounds")
	}
	c.Contexts[context] = append(releases[:index], releases[index+1:]...)
	return nil
}

func (c *Config) GetReleasesForContext(context string) []ReleaseProfile {
	return c.Contexts[context]
}