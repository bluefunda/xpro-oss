package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runMigrate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRun_Usage(t *testing.T) {
	code, _, stderr := runMigrate(t)
	if code != 1 || !strings.Contains(stderr, "usage: migrate") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestRun_UnreadableSQLFile(t *testing.T) {
	code, _, stderr := runMigrate(t, filepath.Join(t.TempDir(), "missing.sql"))
	if code != 1 || !strings.HasPrefix(stderr, "read:") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestRun_ConfigError(t *testing.T) {
	t.Chdir(t.TempDir()) // no config.yaml here
	sqlFile := filepath.Join(t.TempDir(), "m.sql")
	_ = os.WriteFile(sqlFile, []byte("SELECT 1"), 0o600)

	code, _, stderr := runMigrate(t, sqlFile)
	if code != 1 || !strings.HasPrefix(stderr, "config:") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestRun_DBError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = os.WriteFile("config.yaml", []byte("target:\n  db: mysql\n"), 0o600)
	_ = os.WriteFile("m.sql", []byte("SELECT 1"), 0o600)

	code, _, stderr := runMigrate(t, "m.sql")
	if code != 1 || !strings.HasPrefix(stderr, "db:") || !strings.Contains(stderr, "unsupported") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}
