package media

import (
	"image"
	"image/color"
	"image/gif"
	"io"
)

func encodeTinyGIF(w io.Writer) error {
	pal := color.Palette{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
	return gif.Encode(w, img, nil)
}
