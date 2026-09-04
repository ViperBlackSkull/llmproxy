package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	path := writeTempEnv(t, `
# comment line
LLMPROXY_TEST_KEY=test-key-123

  LLMPROXY_SPACED=value-with-spaces
LLMPROXY_QUOTED="double quoted value"
LLMPROXY_SINGLE='single quoted value'
LLMPROXY_EMPTY=
`+"\r\nLLMPROXY_CRLF=crlf-value")

	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}

	for key, want := range map[string]string{
		"LLMPROXY_TEST_KEY": "test-key-123",
		"LLMPROXY_SPACED":   "value-with-spaces",
		"LLMPROXY_QUOTED":   "double quoted value",
		"LLMPROXY_SINGLE":   "single quoted value",
		"LLMPROXY_EMPTY":    "",
		"LLMPROXY_CRLF":     "crlf-value",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLoadEnvFileEnvWins(t *testing.T) {
	path := writeTempEnv(t, "LLMPROXY_FILE_VAR=from-file\nLLMPROXY_FILE_OTHER=from-file-too\n")
	t.Setenv("LLMPROXY_FILE_VAR", "from-env")

	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if got := os.Getenv("LLMPROXY_FILE_VAR"); got != "from-env" {
		t.Errorf("LLMPROXY_FILE_VAR = %q, want from-env (env must win over .env)", got)
	}
	if got := os.Getenv("LLMPROXY_FILE_OTHER"); got != "from-file-too" {
		t.Errorf("LLMPROXY_FILE_OTHER = %q, want from-file-too (unset vars load from .env)", got)
	}
}

func TestLoadEnvFileMissingFile(t *testing.T) {
	err := loadEnvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("error should satisfy os.IsNotExist, got: %v", err)
	}
}

func TestLoadEnvFileMalformedLine(t *testing.T) {
	path := writeTempEnv(t, "GOOD=1\nthis-line-has-no-equals\n")
	err := loadEnvFile(path)
	if err == nil {
		t.Fatal("expected error for line without '=' separator")
	}
}
