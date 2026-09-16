package app

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile loads literal KEY=VALUE settings without overriding the process
// environment. Missing files are optional; malformed settings fail startup.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		valid := ok && key != ""
		for i, c := range key {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
				valid = false
			}
		}
		if !valid {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			end := strings.IndexByte(value[1:], value[0])
			if end < 0 {
				return fmt.Errorf("%s:%d: unterminated quoted value", path, line)
			}
			end++
			tail := strings.TrimSpace(value[end+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return fmt.Errorf("%s:%d: unexpected text after quoted value", path, line)
			}
			value = value[1:end]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set environment variable %s: %w", key, err)
			}
		}
	}
	return nil
}
