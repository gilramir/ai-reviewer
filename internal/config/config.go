// Package config stores the little state that belongs to the user rather than
// to a review: currently just a persistent password, if they set one.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the on-disk configuration.
type Config struct {
	// PasswordHash is the encoded digest written by `ai-reviewer password`.
	// Empty means the daemon generates a fresh secret for each run.
	PasswordHash string `json:"passwordHash,omitempty"`
}

// Dir is the configuration directory, honouring XDG_CONFIG_HOME.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ai-reviewer"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ai-reviewer"), nil
}

// Path is the configuration file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the configuration. A missing file is not an error: it simply means
// no password has been set.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes the configuration with owner-only permissions, since it holds a
// password digest.
func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
