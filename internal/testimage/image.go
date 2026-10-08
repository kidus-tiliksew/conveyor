// Package testimage supplies complete, distinct images to artifact fixtures.
package testimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
)

func fixture(seed string) image.Image {
	sum := sha256.Sum256([]byte(seed))
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{sum[0], sum[1], sum[2], 255})
		}
	}
	return img
}
func PNG(seed string) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, fixture(seed)); err != nil {
		panic(err)
	}
	return b.Bytes()
}
func JPEG(seed string) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, fixture(seed), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// GIF returns a complete one-frame GIF. Typed verification retention and the
// legacy evidence role both refuse GIF, so fixtures use it as the refused case.
func GIF(seed string) []byte {
	var b bytes.Buffer
	if err := gif.Encode(&b, fixture(seed), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// webpPixel is a complete 1x1 lossless WebP image.
const webpPixel = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

// WebP returns a complete WebP image; seed is accepted for symmetry only.
func WebP(string) []byte {
	data, err := base64.StdEncoding.DecodeString(webpPixel)
	if err != nil {
		panic(err)
	}
	return data
}

// MP4 returns an ISO BMFF byte sequence whose boxes cover every byte and whose
// mdat box carries the seed, the bounded container shape the recording check
// admits. It is not a decodable video.
func MP4(seed string) []byte {
	payload := []byte(seed)
	if len(payload) == 0 {
		payload = []byte("frame")
	}
	box := func(kind string, body []byte) []byte {
		out := make([]byte, 8, 8+len(body))
		binary.BigEndian.PutUint32(out, uint32(8+len(body)))
		copy(out[4:], kind)
		return append(out, body...)
	}
	return append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), box("mdat", payload)...)
}

// WebM returns an EBML header with the webm doc type followed by a segment
// and a cluster carrying the seed. It is not a decodable video.
func WebM(seed string) []byte {
	out := []byte{0x1a, 0x45, 0xdf, 0xa3, 0x87, 0x42, 0x82, 0x84}
	out = append(out, "webm"...)
	out = append(out, 0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	out = append(out, 0x1f, 0x43, 0xb6, 0x75)
	return append(out, seed...)
}
