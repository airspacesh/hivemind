package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/airspacesh/hivemind/internal/api"
)

// metadataCapture is what a workspace records about where a post or
// message was written, beyond the username, which is always recorded.
type metadataCapture struct {
	hostname  bool
	directory bool
	session   bool
}

var captureAll = metadataCapture{hostname: true, directory: true, session: true}

// UnmarshalYAML reads all, none, or a list of hostname, directory and
// session, through yaml.Unmarshaler.
func (m *metadataCapture) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Value == "all" {
		*m = captureAll
		return nil
	}
	if node.Kind == yaml.ScalarNode && node.Value == "none" {
		*m = metadataCapture{}
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: metadata_capture must be all, none, "+
			"or a list of hostname, directory and session", node.Line)
	}

	var items []string
	if err := node.Decode(&items); err != nil {
		return err
	}
	*m = metadataCapture{}
	fields := map[string]*bool{
		"hostname":  &m.hostname,
		"directory": &m.directory,
		"session":   &m.session,
	}
	for _, item := range items {
		field, ok := fields[item]
		if !ok {
			return fmt.Errorf("line %d: metadata_capture lists %q. Use "+
				"hostname, directory and session", node.Line, item)
		}
		if *field {
			return fmt.Errorf("line %d: metadata_capture lists %s twice",
				node.Line, item)
		}
		*field = true
	}
	return nil
}

// configFile is the YAML config. Every key it does not name is an
// error, since a typo could send posts somewhere unintended.
type configFile struct {
	Workspaces map[string]workspaceFile `yaml:"workspaces"`
}

type workspaceFile struct {
	Server          string           `yaml:"server"`
	Prefix          string           `yaml:"prefix"`
	Auth            *authFile        `yaml:"auth"`
	MetadataCapture *metadataCapture `yaml:"metadata_capture"`
}

type authFile struct {
	Secret string `yaml:"secret"`
}

// workspaceConfig is a configured workspace: the server that hosts it,
// and what the client knows and records for it.
type workspaceConfig struct {
	name   string
	server string
	// prefix is empty when the config leaves it to the server.
	prefix  string
	secret  string
	capture metadataCapture
}

type clientConfig struct {
	path string
	// workspaces are in the order the file lists them.
	workspaces []workspaceConfig
}

var errNoConfig = errors.New("no config file")

func configEnvironment() string {
	return envPrefix + "_CONFIG"
}

func defaultWorkspaceEnvironment() string {
	return envPrefix + "_DEFAULT_WORKSPACE"
}

func timeoutEnvironment() string {
	return envPrefix + "_TIMEOUT"
}

// configPath is the config file to read: the one <PREFIX>_CONFIG names,
// or the built-in path with a leading ~/ taken from the home directory.
func configPath() (string, error) {
	if path := os.Getenv(configEnvironment()); path != "" {
		return path, nil
	}
	rest, found := strings.CutPrefix(defaultConfigPath, "~/")
	if !found {
		return defaultConfigPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding the config file %s: %w",
			defaultConfigPath, err)
	}
	return home + "/" + rest, nil
}

// parseServer checks a server URL, like http://localhost:7139, and
// drops a trailing slash so the API paths join it.
func parseServer(text string) (string, error) {
	if text == "" {
		return "", errors.New("it has no server")
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return "", fmt.Errorf("its server is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("its server %q must start with http:// or "+
			"https://", text)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", fmt.Errorf("its server %q must be a URL like "+
			"http://localhost:7139", text)
	}
	return strings.TrimSuffix(text, "/"), nil
}

func validateSecret(secret string) error {
	if secret == "" {
		return errors.New("its auth.secret is empty")
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] <= ' ' || secret[i] == 0x7f {
			return errors.New("its auth.secret contains whitespace or a " +
				"control character")
		}
	}
	return nil
}

func newWorkspaceConfig(name string, file workspaceFile) (
	workspaceConfig, error,
) {
	if err := api.ValidateWorkspace(name); err != nil {
		return workspaceConfig{}, err
	}
	server, err := parseServer(file.Server)
	if err != nil {
		return workspaceConfig{}, err
	}
	if file.Prefix != "" {
		if err := api.ValidatePrefix(file.Prefix); err != nil {
			return workspaceConfig{}, err
		}
	}

	workspace := workspaceConfig{
		name:    name,
		server:  server,
		prefix:  file.Prefix,
		capture: captureAll,
	}
	if file.Auth != nil {
		if err := validateSecret(file.Auth.Secret); err != nil {
			return workspaceConfig{}, err
		}
		workspace.secret = file.Auth.Secret
	}
	if file.MetadataCapture != nil {
		workspace.capture = *file.MetadataCapture
	}
	return workspace, nil
}

// decodeConfig reads the config text strictly. A YAML map has no
// order, so the workspace names are read again from the document's
// nodes to keep the order the file lists them in.
func decodeConfig(text []byte) (configFile, []string, error) {
	var file configFile
	decoder := yaml.NewDecoder(bytes.NewReader(text))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("it is empty")
		}
		return configFile{}, nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return configFile{}, nil, errors.New("it must hold one YAML document")
	}

	var ordered struct {
		Workspaces yaml.Node `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(text, &ordered); err != nil {
		return configFile{}, nil, err
	}
	var names []string
	content := ordered.Workspaces.Content
	for i := 0; i+1 < len(content); i += 2 {
		names = append(names, content[i].Value)
	}
	return file, names, nil
}

// loadConfig reads and checks the config at path. It returns
// errNoConfig when there is no file.
func loadConfig(path string) (clientConfig, error) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return clientConfig{}, errNoConfig
	}
	if err != nil {
		return clientConfig{}, fmt.Errorf("reading the config: %w", err)
	}
	file, names, err := decodeConfig(text)
	if err != nil {
		return clientConfig{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if len(names) == 0 {
		return clientConfig{}, fmt.Errorf("invalid config %s: workspaces "+
			"must name at least one workspace", path)
	}

	config := clientConfig{path: path}
	owners := map[string]string{}
	holdsSecret := false
	for _, name := range names {
		workspace, err := newWorkspaceConfig(name, file.Workspaces[name])
		if err != nil {
			return clientConfig{}, fmt.Errorf("invalid config %s: "+
				"workspace %s: %w", path, name, err)
		}
		if owner, taken := owners[workspace.prefix]; taken {
			return clientConfig{}, fmt.Errorf("invalid config %s: "+
				"workspaces %s and %s both set prefix %s", path, owner,
				name, workspace.prefix)
		}
		if workspace.prefix != "" {
			owners[workspace.prefix] = name
		}
		holdsSecret = holdsSecret || workspace.secret != ""
		config.workspaces = append(config.workspaces, workspace)
	}

	if !holdsSecret {
		return config, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return clientConfig{}, fmt.Errorf("reading the config: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return clientConfig{}, fmt.Errorf("the config %s holds a secret, "+
			"so only its owner may read it. Run chmod 600 %s", path, path)
	}
	return config, nil
}

func (c clientConfig) find(name string) (workspaceConfig, bool) {
	for _, workspace := range c.workspaces {
		if workspace.name == name {
			return workspace, true
		}
	}
	return workspaceConfig{}, false
}

func (c clientConfig) names() string {
	var names []string
	for _, workspace := range c.workspaces {
		names = append(names, workspace.name)
	}
	return strings.Join(names, ", ")
}

// fallbackWorkspace is the default workspace when
// <PREFIX>_DEFAULT_WORKSPACE names none.
const fallbackWorkspace = "general"

// defaultWorkspace names the workspace a command uses when it is given
// no --workspace: the one <PREFIX>_DEFAULT_WORKSPACE names, or
// fallbackWorkspace.
func defaultWorkspace() string {
	if name := os.Getenv(defaultWorkspaceEnvironment()); name != "" {
		return name
	}
	return fallbackWorkspace
}

// chooseWorkspace finds the workspace for a command that takes no post
// id: the one --workspace names, or else the default.
func (c clientConfig) chooseWorkspace(flag string) (workspaceConfig, error) {
	name := flag
	source := "--workspace"
	if name == "" {
		name = defaultWorkspace()
		source = defaultWorkspaceEnvironment()
	}
	if workspace, ok := c.find(name); ok {
		return workspace, nil
	}

	if flag == "" && os.Getenv(defaultWorkspaceEnvironment()) == "" {
		return workspaceConfig{}, fmt.Errorf("%s configures no workspace "+
			"called %s. Pass --workspace <name> or set %s (configured: %s)",
			c.path, fallbackWorkspace, defaultWorkspaceEnvironment(),
			c.names())
	}
	return workspaceConfig{}, fmt.Errorf("%s names workspace %q, which %s "+
		"does not configure (configured: %s)", source, name, c.path,
		c.names())
}

// missingConfigHelp explains how to write the config at path.
func missingConfigHelp(path string) string {
	return fmt.Sprintf(`there is no config file at %s, so %s is not set up.
To use it, write that file naming each workspace and its server:

workspaces:
  general:
    server: http://<host>:<port>

A workspace can also set prefix (the letters of its post ids, like e),
auth.secret (when its server needs one), and metadata_capture (all, the
default, none, or a list of hostname, directory and session). Run
'%s help' for more.`, path, programName, programName)
}
