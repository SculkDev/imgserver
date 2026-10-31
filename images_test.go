package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"testing"
)

func TestJPEGOrientationAndMetadataRemoval(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 20, 10)), nil); err != nil {
		t.Fatal(err)
	}
	// Minimal little-endian EXIF IFD with orientation 6 (90 degrees clockwise).
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	var input bytes.Buffer
	input.Write(encoded.Bytes()[:2])
	input.Write([]byte{0xff, 0xe1})
	binary.Write(&input, binary.BigEndian, uint16(len(exif)+2))
	input.Write(exif)
	input.Write(encoded.Bytes()[2:])
	f := setup(t)
	c := grant("avatar")
	w := uploadRequest(f.handler, sign(t, c, f.cfg.SigningKey), input.Bytes(), "portrait.jpeg")
	checkStatus(t, w, 201)
	u := decodeUpload(t, w)
	if u.File.Width != 10 || u.File.Height != 20 || u.File.ContentType != "image/jpeg" || u.File.Name != "portrait.jpg" {
		t.Fatalf("image not oriented/normalized: %+v", u.File)
	}
	w = request(f.handler, "GET", "/v1/files/"+u.File.ID+"/content", "", nil, "")
	checkStatus(t, w, 200)
	if bytes.Contains(w.Body.Bytes(), []byte("Exif")) {
		t.Fatal("EXIF metadata retained")
	}
}
