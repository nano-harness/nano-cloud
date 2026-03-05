package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeKeyFromBin(t *testing.T) {
	if got := runtimeKeyFromBin("claude"); got != "claude_code" {
		t.Fatalf("got %q", got)
	}
	if got := runtimeKeyFromBin("opencode"); got != "opencode" {
		t.Fatalf("got %q", got)
	}
	if got := runtimeKeyFromBin("CustomBin"); got != "custombin" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadCLIRuntimeConfigAndEnvExpand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := []byte(`
cli:
  claude_code:
    env:
      FOO: "bar"
      EXP: "${HOME}"
    args: ["--model", "x", "--home", "${HOME}"]
`)
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	home := "/tmp/test-home"
	_ = os.Setenv("HOME", home)

	entry, err := loadCLIRuntimeConfig(cfgPath, "claude_code")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if entry.Env["FOO"] != "bar" {
		t.Fatalf("env foo = %q", entry.Env["FOO"])
	}
	applyEnv(entry.Env)
	if os.Getenv("FOO") != "bar" {
		t.Fatalf("FOO not applied")
	}
	if os.Getenv("EXP") != home {
		t.Fatalf("EXP not expanded/applied: %q", os.Getenv("EXP"))
	}
}
