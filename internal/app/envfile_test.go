package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	const loaded = "HARMONIA_TEST_ENV_LOADED"
	const existing = "HARMONIA_TEST_ENV_EXISTING"
	old, present := os.LookupEnv(loaded)
	os.Unsetenv(loaded)
	t.Cleanup(func() {
		if present {
			os.Setenv(loaded, old)
		} else {
			os.Unsetenv(loaded)
		}
	})
	t.Setenv(existing, "process")
	path := filepath.Join(t.TempDir(), ".env")
	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\ufeff# config\r\nexport "+loaded+" = 'http://host:3689/' # comment\r\n"+existing+"=file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(loaded) != "http://host:3689/" || os.Getenv(existing) != "process" {
		t.Fatal("file load or environment precedence failed")
	}
	if err := os.WriteFile(path, []byte("INVALID LINE secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(path); err == nil {
		t.Fatal("invalid setting accepted")
	}
}
