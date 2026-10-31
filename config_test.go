package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigSecretsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := defaultConfig()
	key := c.SigningKey
	t.Setenv("TEST_IMGSERVER_KEY", key)
	c.SigningKey = "env:TEST_IMGSERVER_KEY"
	write := func() {
		t.Helper()
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	loaded, err := loadConfig(path)
	if err != nil || loaded.SigningKey != key {
		t.Fatalf("env key: %v", err)
	}
	c.SigningKey = "too-short"
	write()
	if _, err = loadConfig(path); err == nil {
		t.Fatal("weak key accepted")
	}
	c.SigningKey = key
	p := c.Profiles["avatar"]
	p.Sizes = []int{64, 64}
	c.Profiles["avatar"] = p
	write()
	if _, err = loadConfig(path); err == nil {
		t.Fatal("duplicate variants accepted")
	}
	c = defaultConfig()
	c.BaseURL = "https://example.com/unhandled-prefix"
	write()
	if _, err = loadConfig(path); err == nil {
		t.Fatal("unhandled path accepted")
	}
	c = defaultConfig()
	c.AllowedOrigins = []string{"*"}
	write()
	if _, err = loadConfig(path); err == nil {
		t.Fatal("wildcard origin accepted")
	}
	c = defaultConfig()
	p = c.Profiles["ticket"]
	p.Alias = true
	c.Profiles["ticket"] = p
	write()
	if _, err = loadConfig(path); err == nil {
		t.Fatal("private public alias accepted")
	}
}
