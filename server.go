package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg   Config
	meta  *Metadata
	store ObjectStore
	slots chan struct{}
}

func newServer(c Config, m *Metadata, store ObjectStore) *Server {
	return &Server{cfg: c, meta: m, store: store, slots: make(chan struct{}, c.MaxConcurrent)}
}

func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	docsRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/uploads", s.upload)
	mux.HandleFunc("GET /v1/files/{id}", s.metadata)
	mux.HandleFunc("DELETE /v1/files/{id}", s.remove)
	mux.HandleFunc("GET /v1/files/{id}/content", s.content)
	mux.HandleFunc("POST /v1/files/{id}/download-url", s.downloadURL)
	mux.HandleFunc("GET /avatar/{alias}", func(w http.ResponseWriter, r *http.Request) { s.alias(w, r, "avatar") })
	mux.HandleFunc("GET /v1/public/{profile}/{alias}", func(w http.ResponseWriter, r *http.Request) { s.alias(w, r, r.PathValue("profile")) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("X-Request-ID", randomID(12))
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.cfg.AllowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Disposition, X-Request-ID")
		}
		if r.Method == http.MethodOptions {
			if origin == "" || !slices.Contains(s.cfg.AllowedOrigins, origin) {
				fail(w, 403, "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(204)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.meta.db.PingContext(ctx); err != nil {
		fail(w, 503, "metadata unavailable")
		return
	}
	if err := s.store.Ping(ctx); err != nil {
		fail(w, 503, "object storage unavailable")
		return
	}
	respond(w, 200, map[string]string{"status": "ready"})
}

func (s *Server) takeSlot(w http.ResponseWriter) bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, 503, "upload capacity reached; retry shortly")
		return false
	}
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	c, err := s.authorize(r, "upload")
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	if !s.takeSlot(w) {
		return
	}
	defer func() { <-s.slots }()
	id := randomID(18)
	oldID, ready, err := s.meta.reserve(r.Context(), c, fingerprint(c), id)
	if err != nil {
		if errors.Is(err, errReplay) {
			fail(w, 409, err.Error())
		} else {
			s.internal(w, err)
		}
		return
	}
	if oldID != "" {
		if !ready {
			fail(w, 409, "this upload is in progress")
			return
		}
		f, err := s.meta.get(r.Context(), oldID)
		if err != nil {
			s.internal(w, err)
			return
		}
		if f.Deleted {
			fail(w, 410, "file was deleted")
			return
		}
		s.uploadResponse(w, f, 200)
		return
	}
	committed := false
	defer func() {
		if !committed {
			s.meta.release(c)
		}
	}()
	p := s.cfg.Profiles[c.Profile]
	limit := p.MaxBytes
	if c.MaxBytes > 0 && c.MaxBytes < limit {
		limit = c.MaxBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "expected multipart/form-data with one file field")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		fail(w, 400, "expected exactly one field named file with a filename")
		return
	}
	name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
	if len(name) > 255 {
		name = "upload"
	}
	tmp, err := os.CreateTemp("", "imgserver-upload-*")
	if err != nil {
		s.internal(w, err)
		return
	}
	defer func() { tmp.Close(); os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(part, limit+1))
	if n > limit {
		fail(w, 413, "file exceeds upload size limit")
		return
	}
	if err != nil {
		fail(w, 400, "could not read uploaded file")
		return
	}
	if n == 0 {
		fail(w, 400, "empty files are not accepted")
		return
	}
	if _, err = mr.NextPart(); err != io.EOF {
		fail(w, 400, "expected exactly one file and a complete multipart body")
		return
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		s.internal(w, err)
		return
	}
	parts, err := s.prepare(tmp, c.Profile, p)
	if err != nil {
		fail(w, 422, err.Error())
		return
	}
	defer closePayloads(parts)
	base := parts["original"].variant
	if p.ImageOnly {
		ext := ".png"
		if base.ContentType == "image/jpeg" {
			ext = ".jpg"
		}
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}
	f := &File{ID: id, Issuer: c.Issuer, Tenant: c.Tenant, Subject: c.Subject, Resource: c.Resource, Profile: c.Profile, Alias: c.Alias, Public: p.Public, Name: name, ContentType: base.ContentType, Size: base.Size, SHA256: base.SHA256, Width: base.Width, Height: base.Height, CreatedAt: time.Now().UTC(), Variants: map[string]Variant{}}
	// Clean up partial S3 writes if a later write or the metadata transaction fails.
	var keys []string
	for variant, part := range parts {
		key := "files/" + id + "/" + variant
		keys = append(keys, key)
		v := part.variant
		v.Key = key
		f.Variants[variant] = v
	}
	if err = s.meta.stage(r.Context(), id, keys); err != nil {
		s.internal(w, err)
		return
	}
	defer func() {
		if committed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := s.cleanObjects(ctx, id, keys); e != nil {
			slog.Error("upload rollback queued for cleanup", "file_id", id, "error", e)
		}
	}()
	for variant, part := range parts {
		key := "files/" + id + "/" + variant
		if err = s.store.Put(r.Context(), key, part.file, part.variant.Size, part.variant.ContentType); err != nil {
			s.internal(w, err)
			return
		}
	}
	if err = s.meta.save(r.Context(), f, c); err != nil {
		if errors.Is(err, errAliasOwned) {
			fail(w, 409, err.Error())
		} else {
			s.internal(w, err)
		}
		return
	}
	committed = true
	s.uploadResponse(w, f, 201)
}

func (s *Server) uploadResponse(w http.ResponseWriter, f *File, status int) {
	f.Public = s.isPublic(f)
	receipt, err := s.receipt(f)
	if err != nil {
		s.internal(w, err)
		return
	}
	respond(w, status, map[string]any{"file": f, "urls": s.urls(f), "receipt": receipt})
}

// A profile can revoke public access to old uploads. Making a profile public
// never publishes files that were originally uploaded privately.
func (s *Server) isPublic(f *File) bool {
	p, ok := s.cfg.Profiles[f.Profile]
	return ok && p.Public && f.Public
}

func (s *Server) urls(f *File) map[string]string {
	u := map[string]string{"content": s.cfg.BaseURL + "/v1/files/" + f.ID + "/content"}
	if f.Public && f.Alias != "" {
		u["alias"] = s.cfg.BaseURL + "/v1/public/" + f.Profile + "/" + f.Alias
		if f.Profile == "avatar" {
			u["alias"] = s.cfg.BaseURL + "/" + f.Profile + "/" + f.Alias
		}
	}
	for name := range f.Variants {
		if name != "original" {
			u[name] = u["content"] + "?size=" + name
		}
	}
	return u
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) *File {
	f, err := s.meta.get(r.Context(), r.PathValue("id"))
	if err == sql.ErrNoRows {
		fail(w, 404, "file not found")
		return nil
	}
	if err != nil {
		s.internal(w, err)
		return nil
	}
	f.Public = s.isPublic(f)
	return f
}

func (s *Server) require(w http.ResponseWriter, r *http.Request, f *File, action string) bool {
	c, err := s.authorize(r, action)
	if err != nil {
		fail(w, 401, err.Error())
		return false
	}
	if !owns(c, f) {
		fail(w, 403, "grant does not match this file")
		return false
	}
	return true
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	f := s.find(w, r)
	if f == nil {
		return
	}
	if !s.require(w, r, f, "read") {
		return
	}
	if f.Deleted {
		fail(w, 404, "file not found")
		return
	}
	respond(w, 200, map[string]any{"file": f, "urls": s.urls(f)})
}

func variantName(r *http.Request) (string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", err
	}
	for key := range q {
		if key != "size" && key != "expires" && key != "signature" {
			return "", errors.New("unknown query parameter")
		}
	}
	if len(q["size"]) > 1 {
		return "", errors.New("size may only appear once")
	}
	if q.Get("size") == "" {
		return "original", nil
	}
	n, err := strconv.Atoi(q.Get("size"))
	if err != nil || strconv.Itoa(n) != q.Get("size") {
		return "", errors.New("invalid size")
	}
	return q.Get("size"), nil
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	f := s.find(w, r)
	if f == nil {
		return
	}
	v, err := variantName(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if !f.Public && !s.validSignature(r, f.ID, v) && !s.require(w, r, f, "read") {
		return
	}
	if f.Deleted {
		fail(w, 404, "file not found")
		return
	}
	s.serve(w, r, f, v, false)
}

func (s *Server) alias(w http.ResponseWriter, r *http.Request, profile string) {
	p, ok := s.cfg.Profiles[profile]
	if !ok || !p.Public || !p.Alias {
		fail(w, 404, "alias not found")
		return
	}
	hash := r.PathValue("alias")
	if profile == "avatar" && !hashPattern.MatchString(hash) {
		fail(w, 404, "avatar not found")
		return
	}
	v, err := variantName(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	f, err := s.meta.byAlias(r.Context(), profile, hash)
	if err == sql.ErrNoRows {
		if profile == "avatar" {
			s.defaultAvatar(w, r, hash, v, p)
			return
		}
		fail(w, 404, "alias not found")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if f.Deleted && profile == "avatar" {
		s.defaultAvatar(w, r, hash, v, p)
		return
	}
	if f.Deleted || !f.Public {
		fail(w, 404, "alias not found")
		return
	}
	s.serve(w, r, f, v, true)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, f *File, name string, alias bool) {
	v, ok := f.Variants[name]
	if !ok {
		fail(w, 400, "size is not available for this file")
		return
	}
	etag := `"` + v.SHA256 + `"`
	if f.Public {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if alias {
			w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		}
	}
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag || r.Header.Get("If-None-Match") == "*" {
		w.WriteHeader(304)
		return
	}
	w.Header().Set("Content-Type", v.ContentType)
	disposition := "attachment"
	if f.Width > 0 {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
		w.WriteHeader(200)
		return
	}
	object, err := s.store.Open(r.Context(), v.Key)
	if err != nil {
		w.Header().Del("ETag")
		w.Header().Set("Cache-Control", "no-store")
		s.internal(w, err)
		return
	}
	defer object.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
	if _, err = io.Copy(w, object); err != nil {
		slog.Error("content transfer failed", "file_id", f.ID, "error", err)
	}
}

func (s *Server) downloadURL(w http.ResponseWriter, r *http.Request) {
	f := s.find(w, r)
	if f == nil {
		return
	}
	if !s.require(w, r, f, "read") {
		return
	}
	if f.Deleted {
		fail(w, 404, "file not found")
		return
	}
	var body struct {
		Size int `json:"size"`
		TTL  int `json:"ttl_seconds"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&body); err != nil {
			fail(w, 400, "invalid JSON body")
			return
		}
		if err := d.Decode(&struct{}{}); err != io.EOF {
			fail(w, 400, "expected one JSON object")
			return
		}
	}
	if body.TTL == 0 {
		body.TTL = 300
	}
	if body.TTL < 1 || body.TTL > 900 {
		fail(w, 400, "ttl_seconds must be between 1 and 900")
		return
	}
	variant := "original"
	if body.Size != 0 {
		variant = strconv.Itoa(body.Size)
	}
	if _, ok := f.Variants[variant]; !ok {
		fail(w, 400, "size is not available for this file")
		return
	}
	exp := time.Now().Add(time.Duration(body.TTL) * time.Second).Unix()
	expiry := strconv.FormatInt(exp, 10)
	sig := base64.RawURLEncoding.EncodeToString(downloadMAC(string(derivedKey(s.cfg.SigningKey, "download")), f.ID, variant, expiry))
	q := url.Values{"expires": {expiry}, "signature": {sig}}
	if variant != "original" {
		q.Set("size", variant)
	}
	respond(w, 200, map[string]any{"url": s.cfg.BaseURL + "/v1/files/" + f.ID + "/content?" + q.Encode(), "expires_at": exp})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	f := s.find(w, r)
	if f == nil {
		return
	}
	if !s.require(w, r, f, "delete") {
		return
	}
	// Hide first so even previously signed URLs stop working immediately.
	if err := s.meta.markDeleted(r.Context(), f); err != nil {
		s.internal(w, err)
		return
	}
	keys := make([]string, 0, len(f.Variants))
	for _, v := range f.Variants {
		keys = append(keys, v.Key)
	}
	if err := s.cleanObjects(r.Context(), f.ID, keys); err != nil {
		s.internal(w, fmt.Errorf("file hidden; retry DELETE to finish storage cleanup: %w", err))
		return
	}
	w.WriteHeader(204)
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	slog.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
	fail(w, 503, "service temporarily unavailable")
}
