package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // register decoder
	"io"
	"net/http"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register decoder
)

// maxBrandingPixels guards against decompression bombs: a tiny file can
// declare enormous dimensions, and decoding allocates width×height×4 bytes
// before we could reject it. 60 MP covers any phone camera.
const maxBrandingPixels = 60_000_000

// badImageError is a user-facing rejection (unsupported or oversized image).
type badImageError struct{ msg string }

func (e badImageError) Error() string { return e.msg }

// storeBrandingImage decodes an uploaded JPEG/PNG/WebP/HEIC, bakes in its
// EXIF orientation, downsizes it so the long edge is at most maxEdge, and
// writes it to dst as a fresh JPEG. Re-encoding from pixels drops every
// byte of metadata — camera EXIF, GPS location, embedded thumbnails — which
// matters because a circle photo is shown to every member.
func (s *Server) storeBrandingImage(ctx context.Context, src io.Reader, maxEdge int, dst string) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return fmt.Errorf("reading upload: %w", err)
	}

	sniff := data[:min(len(data), 512)]
	switch {
	case isHEIF(sniff):
		if data, err = s.heicToJPEG(ctx, data, filepath.Dir(dst)); err != nil {
			return err
		}
	case isAllowedImageType(http.DetectContentType(sniff)):
	default:
		return badImageError{"That file isn't a photo we can use — try a JPEG, PNG, WebP or HEIC"}
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return badImageError{"Couldn't read that photo — it may be damaged"}
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxBrandingPixels {
		return badImageError{"That photo is too large — please use one under 60 megapixels"}
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return badImageError{"Couldn't read that photo — it may be damaged"}
	}

	out := scaleToFit(img, maxEdge)
	if format == "jpeg" {
		out = applyOrientation(out, jpegOrientation(data))
	}

	tmp := dst + ".tmp"
	f, err := os.Create(tmp) //nolint:gosec // dst is built from a generated name
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	if err := jpeg.Encode(f, out, &jpeg.Options{Quality: 85}); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encoding jpeg: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("flushing jpeg: %w", err)
	}

	return os.Rename(tmp, dst)
}

// heicToJPEG runs the existing HEIC transcode (libheif) on a scratch copy.
func (s *Server) heicToJPEG(ctx context.Context, data []byte, dir string) ([]byte, error) {
	tmp, err := os.CreateTemp(dir, "heic-*.heic")
	if err != nil {
		return nil, fmt.Errorf("creating heic scratch file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return nil, fmt.Errorf("writing heic scratch file: %w", errors.Join(werr, cerr))
	}

	jpegPath, err := s.transcodeHEIC(ctx, name)
	if err != nil {
		_ = os.Remove(name)
		return nil, badImageError{"Couldn't convert this photo — switch the camera to JPEG (\"Most compatible\") and try again"}
	}
	defer os.Remove(jpegPath)

	return os.ReadFile(jpegPath) //nolint:gosec // path produced by transcodeHEIC
}

// scaleToFit returns img flattened onto white (JPEG has no alpha) and
// downscaled so neither side exceeds maxEdge; smaller images keep their size.
func scaleToFit(img image.Image, maxEdge int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > maxEdge {
		w = max(1, w*maxEdge/long)
		h = max(1, h*maxEdge/long)
	}

	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, b, xdraw.Over, nil)

	return out
}

// jpegOrientation reads the EXIF Orientation tag (1..8) from a JPEG; 1 when
// absent or unreadable. The standard library decoder ignores it, so a
// portrait phone photo would otherwise come out sideways.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}

	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan / end: no EXIF
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		seg := data[i+4 : min(len(data), i+2+size)]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}

	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}

	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}

	ifd := int(bo.Uint32(t[4:]))
	if ifd+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for e := 0; e < n; e++ {
		off := ifd + 2 + e*12
		if off+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[off:]) == 0x0112 {
			if v := int(bo.Uint16(t[off+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}

	return 1
}

// applyOrientation rotates/flips img so it displays upright for the given
// EXIF orientation.
func applyOrientation(img *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return img
	}

	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}

	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			si := img.PixOffset(x, y)
			di := out.PixOffset(dx, dy)
			copy(out.Pix[di:di+4], img.Pix[si:si+4])
		}
	}

	return out
}
