package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// Linux trays do not consistently display the Title property beside an icon.
func stateIcon(base []byte, title string) []byte {
	if title == "" {
		return base
	}
	img, err := png.Decode(bytes.NewReader(base))
	if err != nil {
		return base
	}
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, out.Bounds(), img, image.Point{}, draw.Src)
	ink := color.RGBA{22, 128, 68, 255}
	if title == "●" {
		ink = color.RGBA{210, 50, 50, 255}
	}
	r := out.Bounds().Dx() / 8
	cx, cy := out.Bounds().Max.X-r-1, out.Bounds().Max.Y-r-1
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				out.Set(x, y, ink)
			}
		}
	}
	var b bytes.Buffer
	if png.Encode(&b, out) != nil {
		return base
	}
	return b.Bytes()
}
