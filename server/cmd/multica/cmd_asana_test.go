package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newAsanaConnectTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connect"}
	cmd.Flags().String("token", "", "")
	cmd.Flags().Bool("token-stdin", false, "")
	cmd.Flags().String("token-file", "", "")
	return cmd
}

func TestResolveAsanaTokenRequiresOneChannel(t *testing.T) {
	cmd := newAsanaConnectTestCmd()
	if _, err := resolveAsanaToken(cmd); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("expected a usage error without any channel, got %v", err)
	}

	cmd = newAsanaConnectTestCmd()
	_ = cmd.Flags().Set("token", "abc")
	_ = cmd.Flags().Set("token-stdin", "true")
	if _, err := resolveAsanaToken(cmd); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestResolveAsanaTokenReadsEachChannel(t *testing.T) {
	cmd := newAsanaConnectTestCmd()
	_ = cmd.Flags().Set("token", "  inline-token \n")
	if got, err := resolveAsanaToken(cmd); err != nil || got != "inline-token" {
		t.Fatalf("inline: got %q, %v", got, err)
	}

	cmd = newAsanaConnectTestCmd()
	_ = cmd.Flags().Set("token-stdin", "true")
	cmd.SetIn(strings.NewReader("stdin-token\n"))
	if got, err := resolveAsanaToken(cmd); err != nil || got != "stdin-token" {
		t.Fatalf("stdin: got %q, %v", got, err)
	}

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = newAsanaConnectTestCmd()
	_ = cmd.Flags().Set("token-file", path)
	if got, err := resolveAsanaToken(cmd); err != nil || got != "file-token" {
		t.Fatalf("file: got %q, %v", got, err)
	}

	cmd = newAsanaConnectTestCmd()
	_ = cmd.Flags().Set("token-stdin", "true")
	cmd.SetIn(strings.NewReader("   \n"))
	if _, err := resolveAsanaToken(cmd); err == nil {
		t.Fatal("expected an error for an empty stdin token")
	}
}
