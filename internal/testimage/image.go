// Package testimage supplies complete, distinct images to artifact fixtures.
package testimage

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
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
