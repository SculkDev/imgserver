package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Exercise the actual AWS SDK over HTTP, including path-style keys and signing.
// This is an S3 protocol test, not a claim that a real AWS account was tested.
func TestS3UploadDownloadDelete(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SESSION_TOKEN", "")
	type object struct {
		data []byte
		ct   string
	}
	objects := map[string]object{}
	var mu sync.Mutex
	methods := map[string]int{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access-key/") {
			t.Error("SDK request is not signed")
			w.WriteHeader(403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		methods[r.Method]++
		if r.Method == "HEAD" && r.URL.Path == "/test-bucket" {
			w.WriteHeader(200)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/test-bucket/files/") {
			t.Errorf("wrong bucket/key: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "PUT":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[r.URL.Path] = object{data, r.Header.Get("Content-Type")}
			hash := md5.Sum(data)
			w.Header().Set("ETag", `"`+hex.EncodeToString(hash[:])+`"`)
			w.WriteHeader(200)
		case "GET":
			o, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", o.ct)
			w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
			w.Write(o.data)
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected S3 method: %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer backend.Close()
	f := setup(t)
	store, err := newObjectStore(context.Background(), StorageConfig{Driver: "s3", Bucket: "test-bucket", Region: "us-east-1", Endpoint: backend.URL, PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	f.app.store = store
	checkStatus(t, request(f.handler, "GET", "/readyz", "", nil, ""), 200)
	c := grant("ticket")
	data := []byte("S3 private attachment bytes\x00\xff")
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), data, "ticket.bin")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	w = request(f.handler, "GET", fmt.Sprintf("/v1/files/%s/content", u.File.ID), accessToken(t, f, u, "read"), nil, "")
	checkStatus(t, w, 200)
	if !bytes.Equal(data, w.Body.Bytes()) {
		t.Fatalf("S3 round trip changed bytes: %q", w.Body.Bytes())
	}
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, accessToken(t, f, u, "delete"), nil, ""), 204)
	mu.Lock()
	defer mu.Unlock()
	if len(objects) != 0 || methods["PUT"] != 1 || methods["GET"] != 1 || methods["DELETE"] != 1 || methods["HEAD"] != 1 {
		t.Fatalf("unexpected S3 operations: %v, objects=%d", methods, len(objects))
	}
}
