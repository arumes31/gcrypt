//go:build ignore

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"os"
	"strings"
)

// createCirclePNG creates a 16x16 image with a colored circle.
func createCirclePNG(c color.RGBA) *image.RGBA {
	const size = 16
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	cx, cy, r := float64(size/2), float64(size/2), float64(size/2-1)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - cx + 0.5
			dy := float64(y) - cy + 0.5
			if dx*dx+dy*dy <= r*r {
				img.SetRGBA(x, y, c)
			}
		}
	}

	return img
}

// createBitmapData creates raw bitmap data from RGBA image data for ICO format
func createBitmapData(img *image.RGBA) []byte {
	// Get image dimensions
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// BITMAPINFOHEADER structure (40 bytes)
	bmiHeader := make([]byte, 40)
	binary.LittleEndian.PutUint32(bmiHeader[0:], 40) // Size of header
	binary.LittleEndian.PutUint32(bmiHeader[4:], uint32(width))
	binary.LittleEndian.PutUint32(bmiHeader[8:], uint32(height*2)) // Height is doubled for XOR + AND masks
	binary.LittleEndian.PutUint16(bmiHeader[12:], 1)               // Planes
	binary.LittleEndian.PutUint16(bmiHeader[14:], 32)              // Bit count (32-bit ARGB)
	binary.LittleEndian.PutUint32(bmiHeader[16:], 0)               // Compression (0 = BI_RGB)
	binary.LittleEndian.PutUint32(bmiHeader[20:], 0)               // Size of image data (can be 0 for uncompressed)
	binary.LittleEndian.PutUint32(bmiHeader[24:], 0)               // XPelsPerMeter
	binary.LittleEndian.PutUint32(bmiHeader[28:], 0)               // YPelsPerMeter
	binary.LittleEndian.PutUint32(bmiHeader[32:], 0)               // ClrUsed
	binary.LittleEndian.PutUint32(bmiHeader[36:], 0)               // ClrImportant

	// XOR mask (actual image data - BGRA format, bottom-to-top)
	xorSize := width * height * 4
	xorMask := make([]byte, xorSize)

	rowSize := width * 4
	if rowSize%4 != 0 {
		// Rows must be DWORD-aligned in ICO format
		rowSize = ((rowSize + 3) / 4) * 4
	}

	// Copy pixels in reverse row order (bottom-to-top)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// Get RGBA from image
			i := img.PixOffset(x, height-1-y) // Flip vertically
			r := img.Pix[i]
			g := img.Pix[i+1]
			b := img.Pix[i+2]
			a := img.Pix[i+3]

			// Store as BGRA in XOR mask
			dstIdx := y*width*4 + x*4
			xorMask[dstIdx] = b   // Blue
			xorMask[dstIdx+1] = g // Green
			xorMask[dstIdx+2] = r // Red
			xorMask[dstIdx+3] = a // Alpha
		}
	}

	// AND mask (1-bit transparency mask - all zeros for full opacity)
	andRowSize := ((width + 31) / 32) * 4 // DWORD aligned
	andSize := andRowSize * height
	andMask := make([]byte, andSize)

	// Combine all parts
	var buf bytes.Buffer
	buf.Write(bmiHeader)
	buf.Write(xorMask)
	buf.Write(andMask)

	return buf.Bytes()
}

// createICO wraps bitmap data in an ICO container.
func createICO(img *image.RGBA) []byte {
	// Create the bitmap data
	bitmapData := createBitmapData(img)

	// ICO header (6 bytes)
	header := make([]byte, 6)
	binary.LittleEndian.PutUint16(header[0:], 0) // Reserved, must be 0
	binary.LittleEndian.PutUint16(header[2:], 1) // Resource type (1 = ICO)
	binary.LittleEndian.PutUint16(header[4:], 1) // Number of images

	// Directory entry (16 bytes)
	entry := make([]byte, 16)
	entry[0] = 16                                                          // Width (16px, 0 means 256px)
	entry[1] = 16                                                          // Height (16px, 0 means 256px)
	entry[2] = 0                                                           // Colors (0 means more than 256)
	entry[3] = 0                                                           // Reserved
	binary.LittleEndian.PutUint16(entry[4:], 1)                            // Color planes
	binary.LittleEndian.PutUint16(entry[6:], 32)                           // Bits per pixel
	binary.LittleEndian.PutUint32(entry[8:], uint32(len(bitmapData))) // Size of image data (resource only)
	binary.LittleEndian.PutUint32(entry[12:], 6+16)                   // Offset to image data

	var buf bytes.Buffer
	buf.Write(header)
	buf.Write(entry)
	buf.Write(bitmapData)
	return buf.Bytes()
}

func main() {
	colors := map[string]color.RGBA{
		"green": {R: 0, G: 200, B: 0, A: 255},
		"blue":  {R: 0, G: 100, B: 255, A: 255},
		"red":   {R: 220, G: 0, B: 0, A: 255},
		"gray":  {R: 128, G: 128, B: 128, A: 255},
	}

	for name, c := range colors {
		img := createCirclePNG(c)
		icoData := createICO(img)
		b64 := base64.StdEncoding.EncodeToString(icoData)
		title := strings.Title(name)
		fmt.Printf("// icon%sICO\nvar icon%sICO = mustDecodeBase64(\n\t\"%s\",\n)\n\n",
			title, title, b64)

		os.WriteFile(name+"_new.ico", icoData, 0644)
	}
}
