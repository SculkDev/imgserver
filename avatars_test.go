package main

import (
	"bytes"
	"image"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestMissingAvatarGeneratesDeterministicPNG(t *testing.T) {
	f := setup(t)
	const hash = "b58996c504c5638798eb6b511e6f49af"
	path := "/avatar/" + hash
	w := request(f.handler, "GET", path, "", nil, "")
	checkStatus(t, w, 200)
	img, format, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || format != "png" || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("invalid default avatar: %s, %v", format, err)
	}
	if w.Header().Get("Content-Type") != "image/png" || !strings.Contains(w.Header().Get("Content-Disposition"), "inline") || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatalf("incorrect avatar headers: %v", w.Header())
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
		t.Fatal("incorrect content length")
	}
	repeated := request(f.handler, "GET", path, "", nil, "")
	checkStatus(t, repeated, 200)
	if !bytes.Equal(w.Body.Bytes(), repeated.Body.Bytes()) || w.Header().Get("ETag") != repeated.Header().Get("ETag") {
		t.Fatal("same MD5 generated a different avatar")
	}
	otherServer := setup(t)
	repeated = request(otherServer.handler, "GET", path, "", nil, "")
	if !bytes.Equal(w.Body.Bytes(), repeated.Body.Bytes()) {
		t.Fatal("avatar depends on server state")
	}
	other := request(f.handler, "GET", "/avatar/ff5c6a5dcbd6ac394c2d0cdf2dbbcde4", "", nil, "")
	checkStatus(t, other, 200)
	if bytes.Equal(w.Body.Bytes(), other.Body.Bytes()) {
		t.Fatal("different example MD5s generated identical avatars")
	}
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx()/2; x++ {
			if img.At(x, y) != img.At(img.Bounds().Dx()-1-x, y) {
				t.Fatal("identicon is not horizontally mirrored")
			}
		}
	}
	for _, table := range []string{"files", "aliases"} {
		var count int
		if err := f.app.meta.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("generation wrote to %s: %d, %v", table, count, err)
		}
	}
	if _, err := os.Stat(f.cfg.Storage.Directory); !os.IsNotExist(err) {
		t.Fatal("generation created object storage files")
	}
}

func TestGeneratedAvatarSizesHEADAndConditionalRequests(t *testing.T) {
	f := setup(t)
	const hash = "ff5c6a5dcbd6ac394c2d0cdf2dbbcde4"
	path := "/avatar/" + hash + "?size=128"
	w := request(f.handler, "GET", path, "", nil, "")
	checkStatus(t, w, 200)
	img, _, err := image.Decode(bytes.NewReader(w.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 128 || img.Bounds().Dy() != 128 {
		t.Fatalf("wrong generated size: %v", err)
	}
	head := request(f.handler, "HEAD", path, "", nil, "")
	checkStatus(t, head, 200)
	if head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") || head.Header().Get("ETag") != w.Header().Get("ETag") {
		t.Fatal("HEAD does not match GET")
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	f.handler.ServeHTTP(cached, r)
	checkStatus(t, cached, 304)
	if cached.Body.Len() != 0 {
		t.Fatal("304 response has a body")
	}
	generic := request(f.handler, "GET", "/v1/public/avatar/"+hash+"?size=128", "", nil, "")
	checkStatus(t, generic, 200)
	if !bytes.Equal(w.Body.Bytes(), generic.Body.Bytes()) {
		t.Fatal("generic alias route generated a different avatar")
	}
	for _, query := range []string{"?size=999", "?size=0", "?size=-1", "?size=0128", "?size=128&size=64", "?size=hello", "?unexpected=true"} {
		checkStatus(t, request(f.handler, "GET", "/avatar/"+hash+query, "", nil, ""), 400)
	}
	for _, invalid := range []string{"not-an-md5", "B58996C504C5638798EB6B511E6F49AF", "b58996c504c5638798eb6b511e6f49a"} {
		checkStatus(t, request(f.handler, "GET", "/avatar/"+invalid, "", nil, ""), 404)
	}
}

func TestUploadReplacesGeneratedAvatarAndDeleteRestoresIt(t *testing.T) {
	f := setup(t)
	c := grant("avatar")
	c.Alias = "b58996c504c5638798eb6b511e6f49af"
	path := "/avatar/" + c.Alias + "?size=64"
	fallback := request(f.handler, "GET", path, "", nil, "")
	checkStatus(t, fallback, 200)
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), pngImage(t, 80, 40), "avatar.png")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("If-None-Match", fallback.Header().Get("ETag"))
	uploaded := httptest.NewRecorder()
	f.handler.ServeHTTP(uploaded, r)
	checkStatus(t, uploaded, 200)
	if bytes.Equal(fallback.Body.Bytes(), uploaded.Body.Bytes()) || fallback.Header().Get("ETag") == uploaded.Header().Get("ETag") {
		t.Fatal("uploaded avatar did not replace generated avatar")
	}
	checkStatus(t, request(f.handler, "DELETE", "/v1/files/"+u.File.ID, accessToken(t, f, u, "delete"), nil, ""), 204)
	restored := request(f.handler, "GET", path, "", nil, "")
	checkStatus(t, restored, 200)
	if !bytes.Equal(fallback.Body.Bytes(), restored.Body.Bytes()) {
		t.Fatal("deletion did not restore the original deterministic avatar")
	}
	checkStatus(t, request(f.handler, "GET", "/v1/files/"+u.File.ID+"/content", "", nil, ""), 404)
}

func TestAvatarFallbackDoesNotHideDatabaseErrorsOrBypassPrivacy(t *testing.T) {
	f := setup(t)
	const hash = "b58996c504c5638798eb6b511e6f49af"
	p := f.app.cfg.Profiles["avatar"]
	p.Public = false
	p.Alias = false
	f.app.cfg.Profiles["avatar"] = p
	checkStatus(t, request(f.handler, "GET", "/avatar/"+hash, "", nil, ""), 404)
	p.Public = true
	p.Alias = true
	f.app.cfg.Profiles["avatar"] = p
	f.app.cfg.Profiles["other-public"] = Profile{Public: true, Alias: true, MaxBytes: 1024}
	checkStatus(t, request(f.handler, "GET", "/v1/public/other-public/"+hash, "", nil, ""), 404)
	if err := f.app.meta.db.Close(); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, request(f.handler, "GET", "/avatar/"+hash, "", nil, ""), 503)
}
