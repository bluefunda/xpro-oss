// Package config loads xpro-oss's configuration from config.yaml, with
// ${VAR} environment-variable expansion. It is plain data and does no I/O
// beyond reading the file; SAP BTP binding detection lives in package btp.
// Every field has a non-BTP default so xpro-oss runs unmodified on AWS, GCP,
// or bare metal — BTP is opt-in, never required.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"regexp"

	"gopkg.in/yaml.v3"
)

// AppConfig is the root configuration for xpro-oss.
type AppConfig struct {
	Source  SourceConfig   `yaml:"source"`
	Target  TargetConfig   `yaml:"target"`
	Mapping []MappingField `yaml:"mapping"`

	// Workers bounds how many batches process concurrently. Defaults to 8.
	Workers int `yaml:"workers"`
}

// SourceConfig describes where XML files are read from.
type SourceConfig struct {
	// Type selects the source implementation: "local" (default) or "sftp".
	Type string     `yaml:"type"`
	Path string     `yaml:"path"`
	SFTP SFTPConfig `yaml:"sftp"`
}

// SFTPConfig configures the SFTP source, either directly or via BTP.
type SFTPConfig struct {
	// Destination is an optional SAP BTP Destination Service entry name.
	// When set and a BTP Connectivity + Destination binding is present,
	// host/user/password are resolved from the Destination Service and
	// tunneled through the BTP Connectivity SOCKS5 proxy — the same
	// on-prem access pattern used by BlueFunda's BTP services.
	Destination string `yaml:"destination"`

	// Direct connection fields, used when Destination is empty (or no BTP
	// binding is available) — the portable, non-BTP path.
	Host           string `yaml:"host"`
	Port           string `yaml:"port"`
	User           string `yaml:"user"`
	Password       string `yaml:"password"`
	PrivateKeyPath string `yaml:"privateKeyPath"`

	// KnownHosts is the path to an OpenSSH known_hosts file used to verify
	// the server's host key. When empty, host keys are NOT verified (and a
	// warning is logged) — set it for any untrusted network.
	KnownHosts string `yaml:"knownHosts"`
}

// TargetConfig describes the database records are inserted into.
type TargetConfig struct {
	// DB selects the driver: "postgres" (default), "oracle" or "hana".
	DB string `yaml:"db"`

	// Table is the target table name, optionally schema-qualified
	// ("schema.table") for Postgres.
	Table string `yaml:"table"`

	// BatchSize overrides the driver's DB-aware default (0 = use default).
	BatchSize int `yaml:"batchSize"`

	// Destination is an optional SAP BTP Destination Service entry name
	// (Oracle and HANA only). When set and a BTP Connectivity + Destination binding
	// is present, host/port/user/password/service name are resolved from
	// the Destination Service, and traffic is tunneled through the BTP
	// Connectivity SOCKS5 proxy when the destination's ProxyType is
	// "OnPremise" — the same pattern used by BlueFunda's BTP services. Direct fields below
	// are used when Destination is empty.
	Destination string `yaml:"destination"`

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// Database is the Postgres database name, the Oracle service name, or
	// the HANA tenant database name (optional on HANA Cloud).
	Database string `yaml:"database"`
	Schema   string `yaml:"schema"`
	// SSLMode: postgres default "require" (lib/pq values); hana accepts
	// "require" (default, verified TLS), "skip-verify" or "disable".
	// Ignored by oracle.
	SSLMode string `yaml:"sslmode"`
}

// MappingField maps one value onto one target column. Exactly one of XPath
// or Const should be set: XPath extracts from the source file (an empty or
// absent XPath with no Const stores the entire raw file content); Const
// supplies a fixed literal instead — combine with ${VAR} expansion for e.g. a
// run ID that varies per deployment without varying per record. Const is a
// pointer so that an explicitly empty value (`const: ""`, or `${VAR}` that
// expands to nothing) stores an empty string rather than the raw file. Const
// takes precedence if both are set.
type MappingField struct {
	Column string  `yaml:"column"`
	XPath  string  `yaml:"xpath"`
	Const  *string `yaml:"const"`
}

// Load reads config.yaml (path defaults to "config.yaml"), rejects unknown
// keys (a typo like `xpth:` must not silently change what a mapping does),
// and expands ${VAR} references in every string field against the process
// environment. Expansion runs after parsing, so a value containing quotes,
// colons or newlines cannot alter the document's structure.
func Load(path string) (*AppConfig, error) {
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg AppConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	expandStrings(reflect.ValueOf(&cfg).Elem())

	if cfg.Source.Type == "" {
		cfg.Source.Type = "local"
	}
	if cfg.Target.DB == "" {
		cfg.Target.DB = "postgres"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	return &cfg, nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references with the corresponding environment
// variable value. An unset variable expands to the empty string and is
// logged, since an empty password or path is almost never what was meant.
func expandEnv(in string) string {
	return envPattern.ReplaceAllStringFunc(in, func(match string) string {
		name := match[2 : len(match)-1]
		v, ok := os.LookupEnv(name)
		if !ok {
			slog.Warn("config references an unset environment variable", "var", name)
		}
		return v
	})
}

// expandStrings applies expandEnv to every string (and *string) reachable
// from v, recursing through structs and slices.
func expandStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(expandEnv(v.String()))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			expandStrings(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			expandStrings(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			expandStrings(v.Index(i))
		}
	}
}
