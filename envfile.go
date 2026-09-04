package main

import (
	"fmt"
	"os"
	"strings"
)

// loadEnvFile parses a KEY=VALUE .env file and exports each entry into the
// process environment. Variables already set in the real environment always
// win — .env only fills gaps, it never overrides. Blank lines and lines
// starting with '#' are skipped; surrounding whitespace and quotes around
// values are stripped. A missing file is NOT an error for the caller to
// decide (callers treat it as a no-op via os.IsNotExist).
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s:%d: missing '=' separator", path, i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: empty key", path, i+1)
		}
		value = strings.TrimSpace(value)
		// Strip optional surrounding quotes (single or double)
		if len(value) >= 2 &&
			((value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		// Environment wins over .env — never overwrite an existing var
		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
	}
	return nil
}
