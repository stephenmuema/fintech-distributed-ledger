package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadEnvFile reads KEY=VALUE lines from a .env file into the process environment. Variables that
// are already set win, so a deployment's real environment always overrides the file. A missing
// file is not an error. Supported: comments (#), blank lines, an optional `export ` prefix, and
// single- or double-quoted values.
func LoadEnvFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- the path is operator-supplied deployment configuration
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i]) // trailing comment on an unquoted value
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
