package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Claims struct {
	jwt.RegisteredClaims
	Action   string `json:"action"`
	Tenant   string `json:"tenant"`
	Resource string `json:"resource"`
	Profile  string `json:"profile,omitempty"`
	Alias    string `json:"alias,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) authorize(r *http.Request, action string) (*Claims, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 8192 {
		return nil, errors.New("bearer token required")
	}
	return s.parseGrant(strings.TrimPrefix(header, "Bearer "), action)
}

func (s *Server) parseGrant(raw, action string) (*Claims, error) {
	c := &Claims{}
	_, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		return []byte(s.cfg.SigningKey), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("imgserver"), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return nil, errors.New("invalid or expired grant")
	}
	if c.Action != action || !namePattern.MatchString(c.Issuer) || c.Subject == "" || !namePattern.MatchString(c.Tenant) || c.Resource == "" || len(c.Resource) > 256 || c.IssuedAt == nil || c.ID == "" || len(c.ID) > 128 || c.ExpiresAt.Time.Sub(c.IssuedAt.Time) > 15*time.Minute || !c.ExpiresAt.After(c.IssuedAt.Time) {
		return nil, errors.New("invalid grant scope or lifetime (maximum 15 minutes)")
	}
	if action == "upload" {
		p, ok := s.cfg.Profiles[c.Profile]
		if !ok {
			return nil, errors.New("unknown profile")
		}
		if c.MaxBytes < 0 {
			return nil, errors.New("invalid max_bytes")
		}
		if c.Alias != "" && (!p.Alias || !namePattern.MatchString(c.Alias) || (c.Profile == "avatar" && !hashPattern.MatchString(c.Alias))) {
			return nil, errors.New("invalid alias")
		}
	} else if c.FileID == "" {
		return nil, errors.New("file_id is required")
	}
	return c, nil
}

func owns(c *Claims, f *File) bool {
	return c.FileID == f.ID && c.Issuer == f.Issuer && c.Tenant == f.Tenant && c.Resource == f.Resource
}

type Receipt struct {
	jwt.RegisteredClaims
	FileID   string `json:"file_id"`
	Tenant   string `json:"tenant"`
	Resource string `json:"resource"`
	Profile  string `json:"profile"`
	Alias    string `json:"alias,omitempty"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

func (s *Server) receipt(f *File) (string, error) {
	now := time.Now()
	c := Receipt{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "imgserver", Audience: jwt.ClaimStrings{f.Issuer}, Subject: f.Subject, ID: f.ID, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))},
		FileID:           f.ID, Tenant: f.Tenant, Resource: f.Resource, Profile: f.Profile, Alias: f.Alias, SHA256: f.SHA256, Size: f.Size,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(derivedKey(s.cfg.SigningKey, "receipt"))
}

// Purpose-separated subkeys allow one configured secret without token confusion.
func derivedKey(key, purpose string) []byte {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte("imgserver-v1:" + purpose))
	return h.Sum(nil)
}

func downloadMAC(key, id, variant, expiry string) []byte {
	h := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(h, "imgserver-download-v1\n%s\n%s\n%s", id, variant, expiry)
	return h.Sum(nil)
}

func (s *Server) validSignature(r *http.Request, id, variant string) bool {
	q := r.URL.Query()
	if len(q["expires"]) != 1 || len(q["signature"]) != 1 {
		return false
	}
	e := q.Get("expires")
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(q.Get("signature"))
	return err == nil && hmac.Equal(sig, downloadMAC(string(derivedKey(s.cfg.SigningKey, "download")), id, variant, e))
}
