package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_ExpandsEnvAndParses(t *testing.T) {
	t.Setenv("XPRO_TEST_PW", "s3cret")
	p := writeConfig(t, `
source:
  type: sftp
  path: /in
  sftp:
    host: sftp.example
    password: "${XPRO_TEST_PW}"
target:
  db: hana
  table: records
  batchSize: 42
  host: hana.example
  port: 443
  user: u
  password: "${XPRO_TEST_PW}"
  database: TENANT
  sslmode: skip-verify
workers: 3
mapping:
  - column: a
    xpath: /Root/A
  - column: run
    const: "${XPRO_TEST_UNSET}"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Type != "sftp" || cfg.Source.SFTP.Password != "s3cret" || cfg.Source.SFTP.Host != "sftp.example" {
		t.Errorf("source = %+v", cfg.Source)
	}
	tg := cfg.Target
	if tg.DB != "hana" || tg.BatchSize != 42 || tg.Port != 443 || tg.Password != "s3cret" || tg.Database != "TENANT" || tg.SSLMode != "skip-verify" {
		t.Errorf("target = %+v", tg)
	}
	if cfg.Workers != 3 || len(cfg.Mapping) != 2 {
		t.Errorf("workers=%d mapping=%v", cfg.Workers, cfg.Mapping)
	}
	if c := cfg.Mapping[1].Const; c == nil || *c != "" {
		t.Errorf("unset var should expand to an explicit empty const, got %v", c)
	}
	if cfg.Mapping[0].Const != nil {
		t.Error("a mapping without const must leave Const nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "mapping: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Type != "local" || cfg.Target.DB != "postgres" || cfg.Workers != 8 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoad_DefaultPathAndErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "read config.yaml") {
		t.Errorf("missing default file: err = %v", err)
	}
	if _, err := Load(writeConfig(t, "source: [unclosed")); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("bad yaml: err = %v", err)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("XPRO_A", "1")
	got := expandEnv("${XPRO_A}-${XPRO_NOPE}-$XPRO_A-${1bad}")
	if got != "1--$XPRO_A-${1bad}" {
		t.Errorf("expandEnv = %q", got)
	}
}

// TestLoad_EnvValueCannotInjectYAML: expansion happens after parsing, so a
// value full of YAML metacharacters stays one opaque string.
func TestLoad_EnvValueCannotInjectYAML(t *testing.T) {
	const nasty = "p\"w: x\n  workers: 99 # \\"
	t.Setenv("XPRO_PW", nasty)
	cfg, err := Load(writeConfig(t, "target:\n  password: \"${XPRO_PW}\"\nworkers: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target.Password != nasty || cfg.Workers != 2 {
		t.Errorf("password=%q workers=%d", cfg.Target.Password, cfg.Workers)
	}
}

func TestLoad_RejectsUnknownKeys(t *testing.T) {
	for name, body := range map[string]string{
		"top level":    "wrokers: 3\n",
		"mapping typo": "mapping:\n  - column: a\n    xpth: /A\n",
		"nested":       "target:\n  hots: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "parse") {
				t.Errorf("err = %v, want a parse error for the unknown key", err)
			}
		})
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil || cfg.Workers != 8 {
		t.Errorf("cfg=%+v err=%v", cfg, err)
	}
}

func TestLoad_KnownHostsAndExplicitEmptyConst(t *testing.T) {
	cfg, err := Load(writeConfig(t, "source:\n  sftp:\n    knownHosts: /etc/ssh/known_hosts\nmapping:\n  - column: c\n    const: \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.SFTP.KnownHosts != "/etc/ssh/known_hosts" {
		t.Errorf("knownHosts = %q", cfg.Source.SFTP.KnownHosts)
	}
	if c := cfg.Mapping[0].Const; c == nil || *c != "" {
		t.Errorf("explicit empty const must be preserved, got %v", c)
	}
}

// TestRepoConfigsLoad guards the shipped example configs against the strict
// (unknown-key-rejecting) loader.
func TestRepoConfigsLoad(t *testing.T) {
	paths, _ := filepath.Glob("../config.yaml")
	more, _ := filepath.Glob("../examples/*/config.yaml")
	for _, p := range append(paths, more...) {
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
