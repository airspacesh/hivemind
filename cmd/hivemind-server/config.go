package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/airspacesh/hivemind/internal/api"
	"github.com/airspacesh/hivemind/internal/store"
)

const (
	defaultConfigPath = "config.yml"
	defaultListen     = "127.0.0.1:7139"
	defaultTimeout    = 5 * time.Second
)

// serverConfig is the server's config file. Every key it does not name
// is an error.
type serverConfig struct {
	DB         string                     `yaml:"db"`
	Listen     string                     `yaml:"listen"`
	Timeout    time.Duration              `yaml:"timeout"`
	Workspaces map[string]workspaceConfig `yaml:"workspaces"`
}

type workspaceConfig struct {
	Prefix      string            `yaml:"prefix"`
	PostEditors store.PostEditors `yaml:"post_editors"`
	Auth        *authConfig       `yaml:"auth"`
}

type authConfig struct {
	Secret string `yaml:"secret"`
}

// workspaceNames lists the configured workspaces' names in order.
func (c serverConfig) workspaceNames() []string {
	var names []string
	for name := range c.Workspaces {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (c serverConfig) workspaces() []api.Workspace {
	var workspaces []api.Workspace
	for _, name := range c.workspaceNames() {
		workspaces = append(workspaces, api.Workspace{
			Name: name, Prefix: c.Workspaces[name].Prefix,
		})
	}
	return workspaces
}

func (c serverConfig) holdsSecret() bool {
	for _, workspace := range c.Workspaces {
		if workspace.Auth != nil {
			return true
		}
	}
	return false
}

func validateSecret(secret string) error {
	if secret == "" {
		return errors.New("is empty")
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] <= ' ' || secret[i] == 0x7f {
			return errors.New("contains whitespace or a control character")
		}
	}
	return nil
}

// expandHome replaces a leading ~/ in path with the home directory.
func expandHome(path string) (string, error) {
	rest, found := strings.CutPrefix(path, "~/")
	if !found {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

func (c *serverConfig) validate() error {
	if c.DB == "" {
		return errors.New("db is required")
	}
	db, err := expandHome(c.DB)
	if err != nil {
		return fmt.Errorf("db %s: %w", c.DB, err)
	}
	if !filepath.IsAbs(db) {
		return fmt.Errorf("db must be an absolute path or start with "+
			"~/: %q", c.DB)
	}
	c.DB = filepath.Clean(db)
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	if c.Timeout < 0 {
		return errors.New("timeout must be greater than zero")
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}

	if len(c.Workspaces) == 0 {
		return errors.New("workspaces must name at least one workspace")
	}
	owners := map[string]string{}
	for _, name := range c.workspaceNames() {
		workspace := c.Workspaces[name]
		if err := api.ValidateWorkspace(name); err != nil {
			return err
		}
		if err := api.ValidatePrefix(workspace.Prefix); err != nil {
			return fmt.Errorf("workspace %s: %w", name, err)
		}
		if owner, taken := owners[workspace.Prefix]; taken {
			return fmt.Errorf("workspaces %s and %s both have prefix %s",
				owner, name, workspace.Prefix)
		}
		owners[workspace.Prefix] = name
		switch workspace.PostEditors {
		case "":
			workspace.PostEditors = store.PostEditorsAnyone
			c.Workspaces[name] = workspace
		case store.PostEditorsAnyone, store.PostEditorsOwner:
		default:
			return fmt.Errorf("workspace %s: post_editors must be %s or "+
				"%s, not %q", name, store.PostEditorsAnyone,
				store.PostEditorsOwner, workspace.PostEditors)
		}
		if workspace.Auth == nil {
			continue
		}

		if err := validateSecret(workspace.Auth.Secret); err != nil {
			return fmt.Errorf("workspace %s: the secret %w", name, err)
		}
	}
	return nil
}

// loadConfig reads and checks the server config at path, where a
// leading ~/ names the home directory.
func loadConfig(path string) (serverConfig, error) {
	path, err := expandHome(path)
	if err != nil {
		return serverConfig{}, fmt.Errorf("reading the server config: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return serverConfig{}, fmt.Errorf("reading the server config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return serverConfig{}, fmt.Errorf("reading the server config: %w", err)
	}
	text, err := io.ReadAll(file)
	if err != nil {
		return serverConfig{}, fmt.Errorf("reading the server config: %w", err)
	}

	var config serverConfig
	decoder := yaml.NewDecoder(bytes.NewReader(text))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("it is empty")
		}
		return serverConfig{}, fmt.Errorf("invalid server config %s: %w",
			path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return serverConfig{}, fmt.Errorf("invalid server config %s: it "+
			"must hold one YAML document", path)
	}
	if err := config.validate(); err != nil {
		return serverConfig{}, fmt.Errorf("invalid server config %s: %w",
			path, err)
	}
	if config.holdsSecret() && info.Mode().Perm()&0o044 != 0 {
		return serverConfig{}, fmt.Errorf("the server config %s holds a "+
			"secret, so it must not be readable by group or others. "+
			"Run chmod 600 %s", path, path)
	}
	return config, nil
}
