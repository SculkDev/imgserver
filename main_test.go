package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestContainerHealthcheck(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/readyz" {
				t.Errorf("wrong healthcheck endpoint: %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		address, _ := url.Parse(backend.URL)
		err := healthcheck(Config{Listen: address.Host})
		backend.Close()
		if (err == nil) != (status == http.StatusOK) {
			t.Fatalf("status %d: healthcheck error %v", status, err)
		}
	}
}

func TestVersionDoesNotRequireConfiguration(t *testing.T) {
	if err := run([]string{"version", "-config", filepath.Join(t.TempDir(), "missing.json")}); err != nil {
		t.Fatal(err)
	}
}

func TestInitKeepsExistingConfigurationAndPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := run([]string{"init", "-config", path}); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SigningKey) < 32 {
		t.Fatal("missing generated key")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("configuration permissions: %o", info.Mode().Perm())
	}
	if err := run([]string{"init", "-config", path}); err == nil {
		t.Fatal("existing configuration overwritten")
	}
	again, err := loadConfig(path)
	if err != nil || again.SigningKey != c.SigningKey {
		t.Fatal("existing signing key changed")
	}
}
