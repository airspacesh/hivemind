package main

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigReadsWorkspacesAndDefaults(t *testing.T) {
	path := writeConfig(t, `
db: /srv/hivemind/hivemind.db
workspaces:
  personal:
    prefix: p
    auth:
      secret: s3cret
  general:
    prefix: e
`, 0o600)
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Listen != defaultListen || config.Timeout != defaultTimeout {
		t.Fatalf("listen %q and timeout %v, want the defaults",
			config.Listen, config.Timeout)
	}
	if got := config.workspaceNames(); !reflect.DeepEqual(
		got, []string{"general", "personal"}) {
		t.Fatalf("workspaces = %v", got)
	}
	if config.Workspaces["personal"].Auth.Secret != "s3cret" ||
		config.Workspaces["general"].Auth != nil {
		t.Fatalf("auth = %+v", config.Workspaces)
	}
}

func TestConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{"misspelt auth key", `
db: /srv/hivemind/hivemind.db
workspaces:
  personal:
    prefix: p
    auth:
      secrte: s3cret
`, "secrte"},
		{"misspelt auth block", `
db: /srv/hivemind/hivemind.db
workspaces:
  personal:
    prefix: p
    auht:
      secret: s3cret
`, "auht"},
		{"unknown top-level key", `
db: /srv/hivemind/hivemind.db
port: 7139
workspaces:
  general:
    prefix: e
`, "port"},
		{"shared prefix", `
db: /srv/hivemind/hivemind.db
workspaces:
  general:
    prefix: e
  other:
    prefix: e
`, "both have prefix e"},
		{"empty secret", `
db: /srv/hivemind/hivemind.db
workspaces:
  personal:
    prefix: p
    auth: {}
`, "secret is empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, test.text, 0o600))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

func TestConfigPathAndDBTakeTheHomeDirectoryForALeadingTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	text := "db: ~/server/hivemind.db\nworkspaces:\n  e:\n    prefix: e\n"
	if err := os.WriteFile(filepath.Join(home, "config.yml"),
		[]byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig("~/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "server/hivemind.db"); config.DB != want {
		t.Fatalf("db = %q, want %q", config.DB, want)
	}
}

func TestConfigHoldingASecretMustBeReadableByItsOwnerAlone(t *testing.T) {
	secret := "db: /srv/hivemind.db\nworkspaces:\n  p:\n    prefix: p\n" +
		"    auth:\n      secret: s3cret\n"
	open := "db: /srv/hivemind.db\nworkspaces:\n  e:\n    prefix: e\n"
	for _, test := range []struct {
		text string
		mode os.FileMode
		ok   bool
	}{
		{secret, 0o600, true},
		{secret, 0o640, false},
		{secret, 0o604, false},
		{open, 0o644, true},
	} {
		_, err := loadConfig(writeConfig(t, test.text, test.mode))
		if (err == nil) != test.ok {
			t.Fatalf("mode %o, secret %v: error %v", test.mode,
				test.text == secret, err)
		}
	}
}

func TestStartupWarnsOfOpenWorkspacesBeyondLoopbackAndLostOnes(t *testing.T) {
	open := serverWorkspace{}
	open.Name = "general"
	protected := serverWorkspace{credential: &[32]byte{}}
	protected.Name = "personal"
	everywhere := &net.TCPAddr{IP: net.IPv4zero, Port: 7139}
	loopback := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7139}

	got := startupWarnings(everywhere,
		[]serverWorkspace{open, protected}, nil)
	if len(got) != 1 || !strings.HasSuffix(got[0], ": general") {
		t.Fatalf("warnings = %q, want one naming only general", got)
	}
	if got := startupWarnings(loopback,
		[]serverWorkspace{open, protected}, nil); len(got) != 0 {
		t.Fatalf("loopback warnings = %q", got)
	}
	if got := startupWarnings(everywhere,
		[]serverWorkspace{protected}, nil); len(got) != 0 {
		t.Fatalf("protected-only warnings = %q", got)
	}
	got = startupWarnings(loopback, nil, []string{"old"})
	if len(got) != 1 || !strings.Contains(got[0], "workspace old") {
		t.Fatalf("warnings = %q, want one naming old", got)
	}
}

func TestTimeoutIsADuration(t *testing.T) {
	config, err := loadConfig(writeConfig(t,
		"db: /srv/hivemind.db\ntimeout: 90s\nworkspaces:\n"+
			"  e:\n    prefix: e\n",
		0o600))
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != 90*time.Second {
		t.Fatalf("timeout = %v, want 90s", config.Timeout)
	}
}
