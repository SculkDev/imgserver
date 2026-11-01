package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type fixture struct {
	app     *Server
	handler http.Handler
	cfg     Config
}

func setup(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	c := defaultConfig()
	c.Database = filepath.Join(root, "meta.db")
	c.Storage.Directory = filepath.Join(root, "objects")
	c.Profiles["ticket-image"] = Profile{ImageOnly: true, MaxBytes: 5 << 20, Sizes: []int{64, 128}}
	m, err := openMetadata(c.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.db.Close() })
	s := newServer(c, m, &LocalStore{Directory: c.Storage.Directory})
	return &fixture{app: s, handler: s.handler(), cfg: c}
}

func grant(profile string) Claims {
	now := time.Now()
	return Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "auth", Subject: "user-1", Audience: jwt.ClaimStrings{"imgserver"}, ID: randomID(18), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute))}, Action: "upload", Tenant: "acme", Resource: "user:1", Profile: profile}
}
func sign(t *testing.T, c Claims, key string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(h http.Handler, method, path, token string, body io.Reader, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func uploadRequest(h http.Handler, token string, data []byte, name string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	p, _ := m.CreateFormFile("file", name)
	p.Write(data)
	m.Close()
	return request(h, "POST", "/v1/uploads", token, &b, m.FormDataContentType())
}
func checkStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
}

type uploaded struct {
	File    File              `json:"file"`
	URLs    map[string]string `json:"urls"`
	Receipt string            `json:"receipt"`
}

func decodeUpload(t *testing.T, w *httptest.ResponseRecorder) uploaded {
	t.Helper()
	var u uploaded
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}
func pngImage(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func accessToken(t *testing.T, f *fixture, u uploaded, action string) string {
	c := grant("")
	c.Action = action
	c.FileID = u.File.ID
	c.Issuer = u.File.Issuer
	c.Tenant = u.File.Tenant
	c.Resource = u.File.Resource
	return sign(t, c, f.cfg.SigningKey)
}

func TestAvatarUploadAliasResizeReceiptAndReplay(t *testing.T) {
	f := setup(t)
	c := grant("avatar")
	hash := md5.Sum([]byte("person@example.com"))
	c.Alias = hex.EncodeToString(hash[:])
	token := sign(t, c, f.cfg.SigningKey)
	w := uploadRequest(f.handler, token, pngImage(t, 300, 180), "avatar.png")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	if !u.File.Public || u.File.Width != 300 || len(u.File.Variants) != 5 || u.Receipt == "" {
		t.Fatalf("unexpected result: %+v", u)
	}
	var receipt Receipt
	_, err := jwt.ParseWithClaims(u.Receipt, &receipt, func(*jwt.Token) (any, error) { return derivedKey(f.cfg.SigningKey, "receipt"), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("imgserver"), jwt.WithAudience("auth"), jwt.WithExpirationRequired())
	if err != nil || receipt.FileID != u.File.ID || receipt.Resource != c.Resource || receipt.Tenant != c.Tenant || receipt.SHA256 != u.File.SHA256 {
		t.Fatalf("receipt: %+v, %v", receipt, err)
	}
	if _, err = f.app.parseGrant(u.Receipt, "upload"); err == nil {
		t.Fatal("receipt accepted as backend grant")
	}
	w = request(f.handler, "GET", "/avatar/"+c.Alias+"?size=128", "", nil, "")
	checkStatus(t, w, 200)
	img, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 128 || img.Bounds().Dy() != 128 {
		t.Fatalf("wrong resized image: %v", err)
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatal("alias cache policy")
	}
	req := httptest.NewRequest("GET", "/avatar/"+c.Alias+"?size=128", nil)
	req.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	f.handler.ServeHTTP(cached, req)
	checkStatus(t, cached, 304)
	w = uploadRequest(f.handler, token, []byte("a retry must not overwrite bytes"), "different.txt")
	checkStatus(t, w, 200)
	if decodeUpload(t, w).File.ID != u.File.ID {
		t.Fatal("replay created another file")
	}
	w = request(f.handler, "GET", "/v1/files/"+u.File.ID, "", nil, "")
	checkStatus(t, w, 401)
	w = request(f.handler, "GET", "/v1/files/"+u.File.ID, accessToken(t, f, u, "read"), nil, "")
	checkStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "object_key") || strings.Contains(w.Body.String(), "files/"+u.File.ID+"/original\"") {
		t.Fatal("storage key leaked")
	}
	w = request(f.handler, "HEAD", "/v1/files/"+u.File.ID+"/content", "", nil, "")
	checkStatus(t, w, 200)
	if w.Body.Len() != 0 {
		t.Fatal("HEAD contains bytes")
	}
}

func TestUserAndProjectUseSameAvatarAPI(t *testing.T) {
	f := setup(t)
	cases := []struct{ name, key, hash, issuer, resource string }{
		{"user", "user@example.com", "b58996c504c5638798eb6b511e6f49af", "auth", "user:1"},
		{"project", "project:550e8400-e29b-41d4-a716-446655440000", "ff5c6a5dcbd6ac394c2d0cdf2dbbcde4", "projects", "project:550e8400-e29b-41d4-a716-446655440000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hash := md5.Sum([]byte(tc.key))
			if hex.EncodeToString(hash[:]) != tc.hash {
				t.Fatal("unexpected identifier digest")
			}
			c := grant("avatar")
			c.Issuer = tc.issuer
			c.Resource = tc.resource
			c.Alias = tc.hash
			w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 96, 48), "avatar.png")
			checkStatus(t, w, 201)
			u := decodeUpload(t, w)
			if u.File.Profile != "avatar" || u.URLs["alias"] != f.cfg.BaseURL+"/avatar/"+tc.hash {
				t.Fatalf("different avatar API: %+v", u)
			}
			if len(u.File.Variants) != 5 {
				t.Fatal("different processing profile")
			}
			w = request(f.handler, "GET", "/avatar/"+tc.hash+"?size=64", "", nil, "")
			checkStatus(t, w, 200)
			img, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
			if err != nil || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 64 {
				t.Fatalf("expected same square resize: %v", err)
			}
		})
	}
}

func TestPrivateDownloadsAuthorizationAndDeletion(t *testing.T) {
	f := setup(t)
	c := grant("ticket")
	c.Issuer = "support"
	c.Resource = "ticket:42"
	token := sign(t, c, f.cfg.SigningKey)
	w := uploadRequest(f.handler, token, []byte("private support attachment"), "invoice.txt")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	path := "/v1/files/" + u.File.ID + "/content"
	checkStatus(t, request(f.handler, "GET", path, "", nil, ""), 401)
	read := accessToken(t, f, u, "read")
	w = request(f.handler, "GET", path, read, nil, "")
	checkStatus(t, w, 200)
	if w.Body.String() != "private support attachment" || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("private bytes/headers incorrect")
	}
	for _, field := range []string{"tenant", "issuer", "resource", "file"} {
		t.Run(field, func(t *testing.T) {
			c := grant("")
			c.Action = "read"
			c.FileID = u.File.ID
			c.Issuer = u.File.Issuer
			c.Tenant = u.File.Tenant
			c.Resource = u.File.Resource
			switch field {
			case "tenant":
				c.Tenant = "other"
			case "issuer":
				c.Issuer = "other"
			case "resource":
				c.Resource = "ticket:43"
			case "file":
				c.FileID = "other"
			}
			checkStatus(t, request(f.handler, "GET", path, sign(t, c, f.cfg.SigningKey), nil, ""), 403)
		})
	}
	w = request(f.handler, "POST", "/v1/files/"+u.File.ID+"/download-url", read, strings.NewReader(`{"ttl_seconds":60}`), "application/json")
	checkStatus(t, w, 200)
	var link struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &link)
	signed, _ := url.Parse(link.URL)
	checkStatus(t, request(f.handler, "GET", signed.RequestURI(), "", nil, ""), 200)
	tampered := *signed
	q := tampered.Query()
	q.Set("expires", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	tampered.RawQuery = q.Encode()
	checkStatus(t, request(f.handler, "GET", tampered.RequestURI(), "", nil, ""), 401)
	checkStatus(t, request(f.handler, "GET", signed.RequestURI()+"&expires=100", "", nil, ""), 401)
	exp := strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10)
	sig := base64.RawURLEncoding.EncodeToString(downloadMAC(string(derivedKey(f.cfg.SigningKey, "download")), u.File.ID, "original", exp))
	checkStatus(t, request(f.handler, "GET", path+"?expires="+exp+"&signature="+sig, "", nil, ""), 401)
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, read, nil, ""), 401)
	del := accessToken(t, f, u, "delete")
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, del, nil, ""), 204)
	checkStatus(t, request(f.handler, "GET", signed.RequestURI(), "", nil, ""), 404)
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, del, nil, ""), 204)
	checkStatus(t, uploadRequest(f.handler, token, []byte("retry"), "a.txt"), 410)
}

func TestPrivateImageSignaturesBindSizeAndFile(t *testing.T) {
	f := setup(t)
	c := grant("ticket-image")
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 300, 180), "image.png")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	read := accessToken(t, f, u, "read")
	w = request(f.handler, "POST", "/v1/files/"+u.File.ID+"/download-url", read, strings.NewReader(`{"size":64}`), "application/json")
	checkStatus(t, w, 200)
	var link struct {
		URL string `json:"url"`
	}
	json.Unmarshal(w.Body.Bytes(), &link)
	signed, _ := url.Parse(link.URL)
	w = request(f.handler, "GET", signed.RequestURI(), "", nil, "")
	checkStatus(t, w, 200)
	img, _, _ := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 38 {
		t.Fatalf("fit: %v", img.Bounds())
	}
	q := signed.Query()
	q.Set("size", "128")
	signed.RawQuery = q.Encode()
	checkStatus(t, request(f.handler, "GET", signed.RequestURI(), "", nil, ""), 401)
	q.Del("size")
	signed.RawQuery = q.Encode()
	checkStatus(t, request(f.handler, "GET", signed.RequestURI(), "", nil, ""), 401)
	checkStatus(t, request(f.handler, "GET", "/v1/public/ticket-image/anything", "", nil, ""), 404)
}

func TestAliasReplacementAndOwnership(t *testing.T) {
	f := setup(t)
	c := grant("avatar")
	c.Issuer = "projects"
	c.Resource = "project:550e8400-e29b-41d4-a716-446655440000"
	hash := md5.Sum([]byte(c.Resource))
	c.Alias = hex.EncodeToString(hash[:])
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 120, 60), "logo.png")
	checkStatus(t, w, 201)
	first := decodeUpload(t, w)
	c.ID = randomID(18)
	c.Subject = "another-admin"
	w = uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 90, 45), "logo2.png")
	checkStatus(t, w, 201)
	second := decodeUpload(t, w)
	if first.File.ID == second.File.ID {
		t.Fatal("immutable file IDs reused")
	}
	w = request(f.handler, "GET", "/avatar/"+c.Alias, "", nil, "")
	checkStatus(t, w, 200)
	img, _, _ := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if img.Bounds().Dx() != 90 {
		t.Fatal("alias not replaced")
	}
	for _, field := range []string{"tenant", "issuer", "resource"} {
		t.Run(field, func(t *testing.T) {
			other := c
			other.ID = randomID(18)
			switch field {
			case "tenant":
				other.Tenant = "other"
			case "issuer":
				other.Issuer = "other"
			case "resource":
				other.Resource = "project:456"
			}
			checkStatus(t, uploadRequest(f.handler, sign(t, other, f.cfg.SigningKey), pngImage(t, 40, 40), "bad.png"), 409)
		})
	}
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+first.File.ID, accessToken(t, f, first, "delete"), nil, ""), 204)
	checkStatus(t, request(f.handler, "GET", "/avatar/"+c.Alias, "", nil, ""), 200)
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+second.File.ID, accessToken(t, f, second, "delete"), nil, ""), 204)
	checkStatus(t, request(f.handler, "GET", "/avatar/"+c.Alias, "", nil, ""), 200)
	other := c
	other.ID = randomID(18)
	other.Tenant = "hostile"
	checkStatus(t, uploadRequest(f.handler, sign(t, other, f.cfg.SigningKey), pngImage(t, 20, 20), "bad.png"), 409)
}

func TestProfileCanRevokePublicAccess(t *testing.T) {
	f := setup(t)
	c := grant("avatar")
	c.Alias = "0123456789abcdef0123456789abcdef"
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 40, 20), "avatar.png")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	p := f.app.cfg.Profiles["avatar"]
	p.Public = false
	p.Alias = false
	f.app.cfg.Profiles["avatar"] = p
	checkStatus(t, request(f.handler, "GET", "/v1/files/"+u.File.ID+"/content", "", nil, ""), 401)
	checkStatus(t, request(f.handler, "GET", "/avatar/"+c.Alias, "", nil, ""), 404)
	w = request(f.handler, "GET", "/v1/files/"+u.File.ID+"/content", accessToken(t, f, u, "read"), nil, "")
	checkStatus(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("revoked public response is cacheable")
	}
	p = f.app.cfg.Profiles["ticket"]
	p.Public = true
	f.app.cfg.Profiles["ticket"] = p
	private := &File{Public: false, Profile: "ticket"}
	if f.app.isPublic(private) {
		t.Fatal("profile change published a private upload")
	}
}

func TestGrantValidation(t *testing.T) {
	f := setup(t)
	tests := map[string]func(*Claims){
		"audience":        func(c *Claims) { c.Audience = jwt.ClaimStrings{"another-app"} },
		"missing exp":     func(c *Claims) { c.ExpiresAt = nil },
		"missing iat":     func(c *Claims) { c.IssuedAt = nil },
		"future iat":      func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"expired":         func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) },
		"too long":        func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"no jti":          func(c *Claims) { c.ID = "" },
		"no subject":      func(c *Claims) { c.Subject = "" },
		"no issuer":       func(c *Claims) { c.Issuer = "" },
		"no tenant":       func(c *Claims) { c.Tenant = "" },
		"no resource":     func(c *Claims) { c.Resource = "" },
		"wrong action":    func(c *Claims) { c.Action = "read" },
		"unknown profile": func(c *Claims) { c.Profile = "unknown" },
		"invalid alias":   func(c *Claims) { c.Alias = "../../a" },
		"bad avatar hash": func(c *Claims) { c.Alias = "not-an-email-md5" },
		"negative size":   func(c *Claims) { c.MaxBytes = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := grant("avatar")
			mutate(&c)
			checkStatus(t, uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), []byte("data"), "f"), 401)
		})
	}
	c := grant("ticket")
	checkStatus(t, uploadRequest(f.handler, sign(t, c, randomID(48)), []byte("data"), "f"), 401)
	bad, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, c).SignedString([]byte(f.cfg.SigningKey))
	checkStatus(t, uploadRequest(f.handler, bad, []byte("data"), "f"), 401)
	bad, _ = jwt.NewWithClaims(jwt.SigningMethodNone, c).SignedString(jwt.UnsafeAllowNoneSignatureType)
	checkStatus(t, uploadRequest(f.handler, bad, []byte("data"), "f"), 401)
}

func TestUploadLimitsAndFailedRequestRetry(t *testing.T) {
	f := setup(t)
	c := grant("ticket")
	c.MaxBytes = 4
	token := sign(t, c, f.cfg.SigningKey)
	checkStatus(t, uploadRequest(f.handler, token, []byte("too large"), "f"), 413)
	checkStatus(t, uploadRequest(f.handler, token, []byte("okay"), "f"), 201)
	c = grant("avatar")
	token = sign(t, c, f.cfg.SigningKey)
	checkStatus(t, uploadRequest(f.handler, token, []byte("<svg onload='alert(1)'></svg>"), "image.svg"), 422)
	checkStatus(t, uploadRequest(f.handler, token, pngImage(t, 20, 10), "works.png"), 201)
	c = grant("avatar")
	f.app.cfg.MaxPixels = 10
	checkStatus(t, uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 20, 10), "big.png"), 422)
	c = grant("ticket")
	checkStatus(t, uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), nil, "empty.txt"), 400)
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	p, _ := m.CreateFormFile("file", "f")
	p.Write([]byte("data"))
	m.WriteField("public", "true")
	m.Close()
	checkStatus(t, request(f.handler, "POST", "/v1/uploads", sign(t, grant("ticket"), f.cfg.SigningKey), &b, m.FormDataContentType()), 400)
	c = grant("ticket")
	c.Alias = "private-alias"
	checkStatus(t, uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), []byte("x"), "f"), 401)
}

type controlledStore struct {
	ObjectStore
	mu         sync.Mutex
	failPut    bool
	failDelete bool
	entered    chan struct{}
	unblock    chan struct{}
	once       sync.Once
}

func (s *controlledStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, ct string) error {
	if s.entered != nil {
		s.once.Do(func() { close(s.entered); <-s.unblock })
	}
	s.mu.Lock()
	fail := s.failPut
	s.mu.Unlock()
	if fail {
		return errors.New("simulated put failure")
	}
	return s.ObjectStore.Put(ctx, key, r, size, ct)
}
func (s *controlledStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	fail := s.failDelete
	s.mu.Unlock()
	if fail {
		return errors.New("simulated delete failure")
	}
	return s.ObjectStore.Delete(ctx, key)
}

func TestConcurrentRetryUsesOneUpload(t *testing.T) {
	f := setup(t)
	store := &controlledStore{ObjectStore: f.app.store, entered: make(chan struct{}), unblock: make(chan struct{})}
	f.app.store = store
	token := sign(t, grant("ticket"), f.cfg.SigningKey)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- uploadRequest(f.handler, token, []byte("first"), "f") }()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	checkStatus(t, uploadRequest(f.handler, token, []byte("second"), "f"), 409)
	close(store.unblock)
	checkStatus(t, <-result, 201)
	checkStatus(t, uploadRequest(f.handler, token, []byte("third"), "f"), 200)
}

func TestStorageFailureRetryAndPersistentCleanup(t *testing.T) {
	f := setup(t)
	store := &controlledStore{ObjectStore: f.app.store, failPut: true}
	f.app.store = store
	token := sign(t, grant("ticket"), f.cfg.SigningKey)
	checkStatus(t, uploadRequest(f.handler, token, []byte("data"), "f"), 503)
	store.failPut = false
	w := uploadRequest(f.handler, token, []byte("data"), "f")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	store.failDelete = true
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, accessToken(t, f, u, "delete"), nil, ""), 503)
	checkStatus(t, request(f.handler, "GET", "/v1/files/"+u.File.ID+"/content", accessToken(t, f, u, "read"), nil, ""), 404)
	var count int
	f.app.meta.db.QueryRow(`SELECT COUNT(*) FROM cleanup`).Scan(&count)
	if count != 1 {
		t.Fatal("deletion not queued")
	}
	store.failDelete = false
	if err := f.app.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.app.meta.db.QueryRow(`SELECT COUNT(*) FROM cleanup`).Scan(&count)
	if count != 0 {
		t.Fatal("cleanup queue was not cleared")
	}
	key := "files/orphan/original"
	if err := store.Put(context.Background(), key, strings.NewReader("orphan"), 6, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := f.app.meta.stage(context.Background(), "orphan", []string{key}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open(context.Background(), key)
	if err != nil {
		t.Fatal("fresh staging deleted")
	}
	r.Close()
	f.app.meta.db.Exec(`UPDATE cleanup SET not_before=0`)
	if err := f.app.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, err = store.Open(context.Background(), key); err == nil {
		r.Close()
		t.Fatal("orphan not cleaned")
	}
	// Metadata and private object keys survive reopening the database.
	other, err := openMetadata(f.cfg.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer other.db.Close()
	saved, err := other.get(context.Background(), u.File.ID)
	if err != nil || !saved.Deleted || saved.Variants["original"].Key == "" {
		t.Fatalf("persistence failed: %v", err)
	}
}

func TestDocsOpenAPIAndCORS(t *testing.T) {
	f := setup(t)
	w := request(f.handler, "GET", "/openapi.json", "", nil, "")
	checkStatus(t, w, 200)
	var spec struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.0.3" || len(spec.Paths) != 10 {
		t.Fatalf("incomplete spec: %s, paths=%d", spec.OpenAPI, len(spec.Paths))
	}
	for _, path := range []string{"/docs", "/docs/"} {
		w = request(f.handler, "GET", path, "", nil, "")
		checkStatus(t, w, 200)
		if !strings.Contains(w.Body.String(), "swagger-ui-bundle.js") || strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Fatal("docs UI disabled by CSP")
		}
	}
	w = request(f.handler, "GET", "/docs/init.js", "", nil, "")
	checkStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "/openapi.json") {
		t.Fatal("initializer missing specification")
	}
	for _, origin := range []string{"http://localhost:3000", "https://untrusted.example"} {
		r := httptest.NewRequest("OPTIONS", "/v1/uploads", nil)
		r.Header.Set("Origin", origin)
		w = httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if origin == "http://localhost:3000" {
			checkStatus(t, w, 204)
			if w.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("cors origin")
			}
		} else {
			checkStatus(t, w, 403)
		}
	}
}
