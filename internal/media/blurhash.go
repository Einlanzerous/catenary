package media

import (
	"image"
	"math"
	"strings"
)

// Blurhash encodes img as a blurhash string with xc by yc components
// (https://github.com/woltapp/blurhash). Computed server-side on ingest so the
// placeholder is the same string on every client.
func Blurhash(img image.Image, xc, yc int) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	factors := make([][3]float64, 0, xc*yc)
	for j := 0; j < yc; j++ {
		for i := 0; i < xc; i++ {
			norm := 2.0
			if i == 0 && j == 0 {
				norm = 1
			}
			var r, g, bl float64
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					basis := norm * math.Cos(math.Pi*float64(i)*float64(x)/float64(w)) *
						math.Cos(math.Pi*float64(j)*float64(y)/float64(h))
					pr, pg, pb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					r += basis * srgbToLinear(float64(pr>>8)/255)
					g += basis * srgbToLinear(float64(pg>>8)/255)
					bl += basis * srgbToLinear(float64(pb>>8)/255)
				}
			}
			s := 1 / float64(w*h)
			factors = append(factors, [3]float64{r * s, g * s, bl * s})
		}
	}

	var sb strings.Builder
	sb.WriteString(b83(uint64((xc-1)+(yc-1)*9), 1))
	maxAC := 0.0
	for _, f := range factors[1:] {
		for _, v := range f {
			maxAC = math.Max(maxAC, math.Abs(v))
		}
	}
	quantMax := 0
	if len(factors) > 1 {
		quantMax = int(math.Max(0, math.Min(82, math.Floor(maxAC*166-0.5))))
		maxAC = float64(quantMax+1) / 166
	} else {
		maxAC = 1
	}
	sb.WriteString(b83(uint64(quantMax), 1))

	dc := factors[0]
	sb.WriteString(b83(uint64(linearToSRGB(dc[0]))<<16|uint64(linearToSRGB(dc[1]))<<8|uint64(linearToSRGB(dc[2])), 4))
	for _, f := range factors[1:] {
		sb.WriteString(b83(uint64(quantAC(f[0]/maxAC))*19*19+uint64(quantAC(f[1]/maxAC))*19+uint64(quantAC(f[2]/maxAC)), 2))
	}
	return sb.String()
}

func quantAC(v float64) int {
	s := math.Copysign(math.Pow(math.Abs(v), 0.5), v)
	return int(math.Max(0, math.Min(18, math.Floor(s*9+9.5))))
}

func srgbToLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func linearToSRGB(v float64) int {
	v = math.Max(0, math.Min(1, v))
	if v <= 0.0031308 {
		return int(v*12.92*255 + 0.5)
	}
	return int((1.055*math.Pow(v, 1/2.4)-0.055)*255 + 0.5)
}

const b83chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~"

func b83(v uint64, n int) string {
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = b83chars[v%83]
		v /= 83
	}
	return string(out)
}
