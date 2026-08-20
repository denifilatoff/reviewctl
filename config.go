package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Config struct {
	Harness        string       `yaml:"harness"`
	Publish        bool         `yaml:"publish"`
	TrustedAuthors []string     `yaml:"trusted_authors"`
	Repositories   []Repository `yaml:"repositories"`
}

type Repository struct {
	Provider   string `yaml:"provider"`
	Repository string `yaml:"repository"`
}

func DecodeConfig(r io.Reader) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return cfg, fmt.Errorf("decode config: %w", err)
		}
		return cfg, fmt.Errorf("config must contain one YAML document")
	}
	if cfg.Harness != "codex" || len(cfg.TrustedAuthors) == 0 || len(cfg.Repositories) == 0 {
		return cfg, fmt.Errorf("config requires harness codex, trusted authors, and repositories")
	}
	for i := range cfg.TrustedAuthors {
		cfg.TrustedAuthors[i] = normalizeGitHubLogin(cfg.TrustedAuthors[i])
		if cfg.TrustedAuthors[i] == "" {
			return cfg, fmt.Errorf("trusted author cannot be empty")
		}
	}
	for i := range cfg.Repositories {
		repo := &cfg.Repositories[i]
		if repo.Provider != "github" || !repositoryPattern.MatchString(repo.Repository) {
			return cfg, fmt.Errorf("repository %q must be a GitHub owner/name", repo.Repository)
		}
		repo.Repository = strings.ToLower(repo.Repository)
	}
	return cfg, nil
}

func defaultPath(variable, fallback, name string) (string, error) {
	base := os.Getenv(variable)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, "reviewctl", name), nil
}

func configPath() (string, error) { return defaultPath("XDG_CONFIG_HOME", ".config", "config.yaml") }

func statePath() (string, error) {
	return defaultPath("XDG_STATE_HOME", ".local/state", "reviewctl.db")
}

const initialConfig = `harness: codex
publish: false
trusted_authors: []
repositories: []
`

func initializePaths() (string, string, bool, error) {
	config, err := configPath()
	if err != nil {
		return "", "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		return "", "", false, fmt.Errorf("create config directory: %w", err)
	}
	created := false
	file, err := os.OpenFile(config, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		created = true
		if _, err = io.WriteString(file, initialConfig); err == nil {
			err = file.Close()
		} else {
			_ = file.Close()
		}
		if err != nil {
			_ = os.Remove(config)
		}
	} else if os.IsExist(err) {
		err = nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("create config: %w", err)
	}
	state, err := statePath()
	if err != nil {
		return "", "", false, err
	}
	store, err := OpenStore(state)
	if err != nil {
		return "", "", false, err
	}
	if err := store.Close(); err != nil {
		return "", "", false, fmt.Errorf("close state: %w", err)
	}
	return config, state, created, nil
}

func loadConfig() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	defer file.Close()
	return DecodeConfig(file)
}
