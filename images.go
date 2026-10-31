package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"
)

const maxEncodedImage = 64 << 20

type payload struct {
	variant Variant
	file    *os.File
}

func closePayloads(parts map[string]payload) {
	for _, p := range parts {
		p.file.Close()
		os.Remove(p.file.Name())
	}
}

func describe(r io.ReadSeeker) (string, int64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err == nil {
		_, err = r.Seek(0, io.SeekStart)
	}
	return hex.EncodeToString(h.Sum(nil)), n, err
}

type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errors.New("encoded image exceeds 64 MiB")
	}
	n, err := w.w.Write(b)
	w.remaining -= int64(n)
	return n, err
}

func encodeImage(img image.Image, format string) (payload, error) {
	f, err := os.CreateTemp("", "imgserver-image-*")
	if err != nil {
		return payload{}, err
	}
	clean := true
	defer func() {
		if clean {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	w := &limitedWriter{w: f, remaining: maxEncodedImage}
	ct := "image/png"
	if format == "jpeg" {
		ct = "image/jpeg"
		err = jpeg.Encode(w, img, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(w, img)
	}
	if err != nil {
		return payload{}, err
	}
	hash, size, err := describe(f)
	if err != nil {
		return payload{}, err
	}
	clean = false
	return payload{file: f, variant: Variant{Size: size, Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), ContentType: ct, SHA256: hash}}, nil
}

func (s *Server) prepare(file *os.File, profileName string, p Profile) (parts map[string]payload, err error) {
	parts = map[string]payload{}
	defer func() {
		if err != nil {
			closePayloads(parts)
		}
	}()
	if !p.ImageOnly {
		sniff := make([]byte, 512)
		n, _ := file.Read(sniff)
		hash, size, e := describe(file)
		if e != nil {
			return nil, e
		}
		parts["original"] = payload{file: file, variant: Variant{Size: size, ContentType: http.DetectContentType(sniff[:n]), SHA256: hash}}
		return parts, nil
	}
	conf, format, err := image.DecodeConfig(file)
	if err != nil || (format != "jpeg" && format != "png" && format != "gif" && format != "webp") {
		return nil, errors.New("expected a JPEG, PNG, GIF, or WebP image")
	}
	if conf.Width < 1 || conf.Height < 1 || int64(conf.Width) > s.cfg.MaxPixels/int64(conf.Height) {
		return nil, fmt.Errorf("image exceeds the %d pixel limit", s.cfg.MaxPixels)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, err := imaging.Decode(file, imaging.AutoOrientation(true))
	if err != nil {
		return nil, errors.New("invalid image data")
	}
	// Re-encoding strips EXIF and other embedded metadata; GIFs use their first frame.
	original, err := encodeImage(img, format)
	if err != nil {
		return nil, err
	}
	parts["original"] = original
	for _, size := range p.Sizes {
		var scaled image.Image
		if profileName == "avatar" {
			scaled = imaging.Fill(img, size, size, imaging.Center, imaging.Lanczos)
		} else {
			scaled = imaging.Fit(img, size, size, imaging.Lanczos)
		}
		part, e := encodeImage(scaled, format)
		if e != nil {
			return parts, e
		}
		parts[strconv.Itoa(size)] = part
	}
	return parts, nil
}

func fingerprint(c *Claims) string {
	// The validated claims are hashed by the caller; distinct grants cannot share a jti.
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
