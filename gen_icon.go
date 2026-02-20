//go:build ignore

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// drawShield draws a pointed-bottom shield outline on a 22x22 NRGBA image.
// The shield is white on transparent, suitable for macOS menu bar template images.
func drawShield() *image.NRGBA {
	const size = 22
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}

	// Shield geometry (in float coordinates):
	// - Top-left corner at (3, 2), top-right at (18, 2)
	// - Slight inward curve at top center (decorative)
	// - Sides taper from top width down to a point at bottom center (11, 20)
	// - Left side curves from (3, 2) through (3, 10) to (11, 20)
	// - Right side curves from (18, 2) through (18, 10) to (11, 20)

	cx := 10.5 // center x
	topY := 2.0
	botY := 20.0
	leftX := 3.0
	rightX := 18.0
	shoulderY := 10.0 // where sides start tapering

	// We'll define the shield outline as a set of line segments and
	// use anti-aliased line drawing.

	// Set a pixel with alpha blending for anti-aliasing
	setPixel := func(x, y int, alpha float64) {
		if x < 0 || x >= size || y < 0 || y >= size {
			return
		}
		a := uint8(math.Round(alpha * 255))
		if a == 0 {
			return
		}
		existing := img.NRGBAAt(x, y)
		// Combine: keep maximum alpha (since we're drawing white on transparent)
		if a > existing.A {
			img.SetNRGBA(x, y, color.NRGBA{R: white.R, G: white.G, B: white.B, A: a})
		}
	}

	// Xiaolin Wu's anti-aliased line drawing
	drawLineAA := func(x0, y0, x1, y1 float64) {
		steep := math.Abs(y1-y0) > math.Abs(x1-x0)
		if steep {
			x0, y0 = y0, x0
			x1, y1 = y1, x1
		}
		if x0 > x1 {
			x0, x1 = x1, x0
			y0, y1 = y1, y0
		}

		dx := x1 - x0
		dy := y1 - y0
		gradient := 0.0
		if dx != 0 {
			gradient = dy / dx
		}

		// First endpoint
		xend := math.Round(x0)
		yend := y0 + gradient*(xend-x0)
		xgap := 1.0 - fpart(x0+0.5)
		xpxl1 := int(xend)
		ypxl1 := int(math.Floor(yend))
		if steep {
			setPixel(ypxl1, xpxl1, (1-fpart(yend))*xgap)
			setPixel(ypxl1+1, xpxl1, fpart(yend)*xgap)
		} else {
			setPixel(xpxl1, ypxl1, (1-fpart(yend))*xgap)
			setPixel(xpxl1, ypxl1+1, fpart(yend)*xgap)
		}
		intery := yend + gradient

		// Second endpoint
		xend = math.Round(x1)
		yend = y1 + gradient*(xend-x1)
		xgap = fpart(x1 + 0.5)
		xpxl2 := int(xend)
		ypxl2 := int(math.Floor(yend))
		if steep {
			setPixel(ypxl2, xpxl2, (1-fpart(yend))*xgap)
			setPixel(ypxl2+1, xpxl2, fpart(yend)*xgap)
		} else {
			setPixel(xpxl2, ypxl2, (1-fpart(yend))*xgap)
			setPixel(xpxl2, ypxl2+1, fpart(yend)*xgap)
		}

		// Main loop
		for x := xpxl1 + 1; x < xpxl2; x++ {
			iy := int(math.Floor(intery))
			f := fpart(intery)
			if steep {
				setPixel(iy, x, 1-f)
				setPixel(iy+1, x, f)
			} else {
				setPixel(x, iy, 1-f)
				setPixel(x, iy+1, f)
			}
			intery += gradient
		}
	}

	// Draw a thick line (draw the line at multiple offsets for ~2px weight)
	drawThickLine := func(x0, y0, x1, y1 float64) {
		dx := x1 - x0
		dy := y1 - y0
		length := math.Sqrt(dx*dx + dy*dy)
		if length == 0 {
			return
		}
		// Normal vector (perpendicular to the line)
		nx := -dy / length
		ny := dx / length

		// Draw at offsets -0.4, 0, +0.4 for approximately 2px width
		for _, offset := range []float64{-0.4, -0.15, 0.15, 0.4} {
			ox := nx * offset
			oy := ny * offset
			drawLineAA(x0+ox, y0+oy, x1+ox, y1+oy)
		}
	}

	// --- Shield outline ---

	// Top edge: flat line from top-left to top-right
	drawThickLine(leftX, topY, rightX, topY)

	// Left side: straight down from top-left to shoulder
	drawThickLine(leftX, topY, leftX, shoulderY)

	// Right side: straight down from top-right to shoulder
	drawThickLine(rightX, topY, rightX, shoulderY)

	// Left taper: from left shoulder to bottom point
	drawThickLine(leftX, shoulderY, cx, botY)

	// Right taper: from right shoulder to bottom point
	drawThickLine(rightX, shoulderY, cx, botY)

	// --- Small cross/plus inside the shield for "guard" feel ---
	// Vertical bar of cross
	midY := 9.0
	crossHalfH := 3.5
	crossHalfW := 2.5
	drawThickLine(cx, midY-crossHalfH, cx, midY+crossHalfH)
	// Horizontal bar of cross
	drawThickLine(cx-crossHalfW, midY, cx+crossHalfW, midY)

	return img
}

func fpart(x float64) float64 {
	return x - math.Floor(x)
}

func main() {
	img := drawShield()

	// Write PNG to file
	f, err := os.Create("icon.png")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create icon.png: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		fmt.Fprintf(os.Stderr, "encode png: %v\n", err)
		os.Exit(1)
	}

	// Also generate the Go source with embedded bytes
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		fmt.Fprintf(os.Stderr, "encode png to buffer: %v\n", err)
		os.Exit(1)
	}

	data := buf.Bytes()

	out, err := os.Create("icon.go")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create icon.go: %v\n", err)
		os.Exit(1)
	}
	defer out.Close()

	fmt.Fprintln(out, "package main")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "// iconPNG is a 22x22 white-on-transparent shield icon for macOS menu bar.")
	fmt.Fprintln(out, "// Generated by gen_icon.go — do not edit manually.")
	fmt.Fprintf(out, "var iconPNG = []byte{")
	for i, b := range data {
		if i%16 == 0 {
			fmt.Fprintf(out, "\n\t")
		}
		fmt.Fprintf(out, "0x%02x, ", b)
	}
	fmt.Fprintln(out, "\n}")
	fmt.Fprintln(out)

	fmt.Printf("Generated icon.go (%d bytes PNG, %d pixels)\n", len(data), 22*22)
}
