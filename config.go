package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Listen         string             `json:"listen"`
	BaseURL        string             `json:"base_url"`
	Database       string             `json:"database"`
	SigningKey     string             `json:"signing_key"`
	AllowedOrigins []string           `json:"allowed_origins"`
	MaxConcurrent  int                `json:"max_concurrent"`
	MaxPixels      int64              `json:"max_pixels"`
	Profiles       map[string]Profile `json:"profiles"`
	Storage        StorageConfig      `json:"storage"`
}

type Profile struct {
	Public    bool  `json:"public"`
	ImageOnly bool  `json:"image_only"`
	MaxBytes  int64 `json:"max_bytes"`
	Sizes     []int `json:"sizes,omitempty"`
	Alias     bool  `json:"alias"`
}

type StorageConfig struct {
	Driver    string `json:"driver"`
	Directory string `json:"directory,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
}

// Secrets may be literal values or env:VARIABLE references. Never expand raw JSON.
func secretValue(s string) string {
	if strings.HasPrefix(s, "env:") {
		return os.Getenv(strings.TrimPrefix(s, "env:"))
	}
	return s
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	if v := os.Getenv("IMGSERVER_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("IMGSERVER_LISTEN"); v != "" {
		c.Listen = v
	}
	if c.Listen == "" {
		c.Listen = ":18080"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return c, errors.New("base_url must be an absolute HTTP(S) origin without path, query, or credentials")
	}
	if c.Database == "" {
		return c, errors.New("database is required")
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.MaxPixels == 0 {
		c.MaxPixels = 25_000_000
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > 64 || c.MaxPixels < 1 || c.MaxPixels > 100_000_000 {
		return c, errors.New("invalid concurrency or pixel limit")
	}
	c.SigningKey = secretValue(c.SigningKey)
	if len(c.SigningKey) < 32 || strings.Contains(strings.ToLower(c.SigningKey), "replace-me") {
		return c, errors.New("signing_key must contain at least 32 secret bytes")
	}
	if len(c.Profiles) == 0 {
		return c, errors.New("at least one profile is required")
	}
	for name, p := range c.Profiles {
		if !namePattern.MatchString(name) || p.MaxBytes < 1 || p.MaxBytes > 1<<30 || len(p.Sizes) > 12 || (p.Alias && !p.Public) || (len(p.Sizes) > 0 && !p.ImageOnly) {
			return c, fmt.Errorf("invalid profile %q", name)
		}
		sizes := map[int]bool{}
		for _, n := range p.Sizes {
			if n < 16 || n > 2048 || sizes[n] {
				return c, fmt.Errorf("invalid/duplicate size in profile %s", name)
			}
			sizes[n] = true
		}
	}
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, fmt.Errorf("invalid allowed origin %q", origin)
		}
	}
	switch c.Storage.Driver {
	case "local":
		if c.Storage.Directory == "" {
			return c, errors.New("local storage directory is required")
		}
	case "s3":
		if c.Storage.Bucket == "" || c.Storage.Region == "" {
			return c, errors.New("S3 bucket and region are required")
		}
		if c.Storage.Endpoint != "" {
			u, err := url.Parse(c.Storage.Endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return c, errors.New("invalid S3 endpoint")
			}
		}
	default:
		return c, errors.New("storage driver must be local or s3")
	}
	return c, nil
}

func defaultConfig() Config {
	return Config{
		Listen: ":18080", BaseURL: "http://localhost:18080", Database: ".data/metadata.db",
		SigningKey: randomID(48), MaxConcurrent: 4, MaxPixels: 25_000_000,
		AllowedOrigins: []string{"http://localhost:3000"},
		Storage:        StorageConfig{Driver: "local", Directory: ".data/objects"},
		Profiles: map[string]Profile{
			"avatar":       {Public: true, ImageOnly: true, MaxBytes: 5 << 20, Sizes: []int{32, 64, 128, 256}, Alias: true},
			"ticket":       {Public: false, MaxBytes: 25 << 20},
			"ticket-image": {Public: false, ImageOnly: true, MaxBytes: 10 << 20, Sizes: []int{128, 512, 1024}},
		},
	}
}
