package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"slices"
	"strconv"
)

const defaultAvatarSize = 256

// Identicons are generated entirely from the lookup key. They need no database
// writes, object storage, external image service, or knowledge of the identifier.
func avatarPNG(hash string, size int) ([]byte, error) {
	seed := sha256.Sum256([]byte("imgserver-avatar-v1:" + hash))
	palette := [...]color.NRGBA{
		{R: 79, G: 70, B: 229, A: 255},
		{R: 2, G: 132, B: 199, A: 255},
		{R: 13, G: 148, B: 136, A: 255},
		{R: 22, G: 163, B: 74, A: 255},
		{R: 217, G: 119, B: 6, A: 255},
		{R: 225, G: 29, B: 72, A: 255},
		{R: 147, G: 51, B: 234, A: 255},
		{R: 71, G: 85, B: 105, A: 255},
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	background := image.NewUniform(color.NRGBA{R: 248, G: 250, B: 252, A: 255})
	foreground := image.NewUniform(palette[int(seed[0])%len(palette)])
	draw.Draw(img, img.Bounds(), background, image.Point{}, draw.Src)
	cell := size / 7
	margin := (size - 5*cell) / 2
	drawCell := func(row, col int) {
		x0, y0 := margin+col*cell, margin+row*cell
		x1 := x0 + cell
		if col == 2 {
			x1 = size - x0
		}
		draw.Draw(img, image.Rect(x0, y0, x1, y0+cell), foreground, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(size-x1, y0, size-x0, y0+cell), foreground, image.Point{}, draw.Src)
	}
	filled := false
	for row := 0; row < 5; row++ {
		for col := 0; col < 3; col++ {
			if seed[1+row*3+col]&1 != 0 {
				drawCell(row, col)
				filled = true
			}
		}
	}
	if !filled {
		drawCell(2, 2)
	}
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&encoded, img); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func (s *Server) defaultAvatar(w http.ResponseWriter, r *http.Request, hash, variant string, profile Profile) {
	size := defaultAvatarSize
	if variant != "original" {
		size, _ = strconv.Atoi(variant) // variantName already validates the canonical integer.
		if !slices.Contains(profile.Sizes, size) {
			fail(w, 400, "size is not available for this profile")
			return
		}
	}
	data, err := avatarPNG(hash, size)
	if err != nil {
		s.internal(w, err)
		return
	}
	checksum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(checksum[:]) + `"`
	w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag || r.Header.Get("If-None-Match") == "*" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `inline; filename="avatar.png"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		w.Write(data)
	}
}
