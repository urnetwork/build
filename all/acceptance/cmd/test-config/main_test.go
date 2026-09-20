package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCLIProcess(t *testing.T) {
	if os.Getenv("URNETWORK_TEST_CONFIG_SUBPROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"test-config"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(3)
}

func TestExplicitSchemaCLIAndNoFallback(t *testing.T) {
	flat := filepath.Join(t.TempDir(), "user-pass.yml")
	vault := filepath.Join(t.TempDir(), "vault.yml")
	if err := os.WriteFile(flat, []byte("user: physical@example.invalid\npass: '  private-fixture-pass  '\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault, []byte(`version: 1
email_verification:
  bypass_domains: [fixture.invalid]
  suppress_account_messages: true
signup:
  network_name_prefix: fixture
  email: {domain: fixture.invalid, local_part_prefix: fixture}
data_plane_account: {email: physical@example.invalid, password: '  private-fixture-pass  '}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name string
		args []string
		want string
		pass bool
	}{
		{"flat-user", []string{"--config", flat, "--schema", "user-pass", "get", "user"}, "physical@example.invalid", true},
		{"flat-pass", []string{"--config", flat, "--schema", "user-pass", "get", "pass"}, "  private-fixture-pass  ", true},
		{"vault-explicit", []string{"--config", vault, "--schema", "data-plane-account", "get", "data_plane_account.password"}, "  private-fixture-pass  ", true},
		{"vault-default-unchanged", []string{"--config", vault, "get", "data_plane_account.email"}, "physical@example.invalid", true},
		{"flat-as-vault", []string{"--config", flat, "--schema", "data-plane-account", "get", "data_plane_account.email"}, "", false},
		{"vault-as-flat", []string{"--config", vault, "--schema", "user-pass", "get", "user"}, "", false},
		{"flat-wrong-key", []string{"--config", flat, "--schema", "user-pass", "get", "data_plane_account.email"}, "", false},
		{"flat-no-validate", []string{"--config", flat, "--schema", "user-pass", "validate"}, "", false},
		{"flat-no-ready", []string{"--config", flat, "--schema", "user-pass", "--ready", "get", "user"}, "", false},
		{"unknown-schema", []string{"--config", flat, "--schema", "unknown", "get", "user"}, "", false},
	} {
		t.Run(entry.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestConfigCLIProcess$", "--"}, entry.args...)
			command := exec.Command(os.Args[0], args...)
			command.Env = append(os.Environ(), "URNETWORK_TEST_CONFIG_SUBPROCESS=1")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if (err == nil) != entry.pass || stdout.String() != entry.want {
				t.Fatal("unexpected schema command result")
			}
			if entry.pass && stderr.Len() != 0 {
				t.Fatal("successful scalar read emitted diagnostics")
			}
			if strings.Contains(stderr.String(), "private-fixture-pass") || strings.Contains(stderr.String(), "physical@example.invalid") {
				t.Fatal("reader diagnostic leaked a scalar")
			}
		})
	}
}
