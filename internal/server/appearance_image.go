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

// maxExifBytes bounds how much metadata is read looking for Orientation.
const maxExifBytes = 64 << 10

// badImageError is a user-facing rejection (unsupported, damaged or
// oversized image) — answered with 422, unlike storage failures.
type badImageError struct{ msg string }

func (e badImageError) Error() string { return e.msg }

var (
	errNotAPhoto  = badImageError{"That file isn't a photo we can use — try a JPEG, PNG, WebP or HEIC"}
	errDamaged    = badImageError{"Couldn't read that photo — it may be damaged"}
	errTooManyPix = badImageError{"That photo is too large — please use one under 60 megapixels"}
)

// uploadedImage is what multipart hands us: seekable and randomly readable,
// backed by memory for small parts and a temp file for large ones, so the
// photo is never buffered a second time.
type uploadedImage interface {
	io.Reader
	io.ReaderAt
	io.Seeker
}

func tooManyPixels(w, h int) bool {
	return w <= 0 || h <= 0 || int64(w)*int64(h) > maxBrandingPixels
}

// storeBrandingImage decodes an uploaded JPEG/PNG/WebP/HEIC, bakes in its
// EXIF orientation, downsizes it so the long edge is at most maxEdge, and
// writes it to dst as a fresh JPEG. Re-encoding from pixels drops every
// byte of metadata — camera EXIF, GPS location, embedded thumbnails — which
// matters because a circle photo is shown to every member.
func (s *Server) storeBrandingImage(ctx context.Context, src uploadedImage, maxEdge int, dst string) error {
	sniff := make([]byte, 512)
	n, err := src.ReadAt(sniff, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading upload: %w", err)
	}
	sniff = sniff[:n]

	var in uploadedImage = src
	switch {
	case isHEIF(sniff):
		// Size-check before handing the file to libheif, which would
		// otherwise decode an arbitrarily large image first.
		w, h, ok := heifDimensions(src)
		if !ok {
			return errDamaged
		}
		if tooManyPixels(w, h) {
			return errTooManyPix
		}
		jpegFile, err := s.heicToJPEG(ctx, src, filepath.Dir(dst))
		if err != nil {
			return err
		}
		defer os.Remove(jpegFile.Name())
		defer jpegFile.Close()
		in = jpegFile
	case isAllowedImageType(http.DetectContentType(sniff)):
	default:
		return errNotAPhoto
	}

	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding upload: %w", err)
	}
	cfg, format, err := image.DecodeConfig(in)
	if err != nil {
		return errDamaged
	}
	if tooManyPixels(cfg.Width, cfg.Height) {
		return errTooManyPix
	}

	orientation := imageOrientation(in, format)

	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding upload: %w", err)
	}
	img, _, err := image.Decode(in)
	if err != nil {
		return errDamaged
	}

	return writeJPEGAtomic(dst, applyOrientation(scaleToFit(img, maxEdge), orientation))
}

// writeJPEGAtomic encodes img to a temp file beside dst and renames it into
// place, so a half-written photo is never visible under dst; the temp file
// is removed on every failure path.
func writeJPEGAtomic(dst string, img image.Image) (err error) {
	tmp := dst + ".tmp"
	f, err := os.Create(tmp) //nolint:gosec // dst is built from a generated name
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		_ = f.Close()
		return fmt.Errorf("encoding jpeg: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("flushing jpeg: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("publishing jpeg: %w", err)
	}

	return nil
}

// heicToJPEG runs the existing HEIC transcode (libheif) on a scratch copy
// and returns the resulting JPEG, open for reading.
func (s *Server) heicToJPEG(ctx context.Context, src io.ReadSeeker, dir string) (*os.File, error) {
	tmp, err := os.CreateTemp(dir, "heic-*.heic")
	if err != nil {
		return nil, fmt.Errorf("creating heic scratch file: %w", err)
	}
	name := tmp.Name()

	_, err = src.Seek(0, io.SeekStart)
	if err == nil {
		_, err = io.Copy(tmp, src)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return nil, fmt.Errorf("writing heic scratch file: %w", err)
	}

	jpegPath, err := s.transcodeHEIC(ctx, name)
	if err != nil {
		_ = os.Remove(name)
		return nil, badImageError{"Couldn't convert this photo — switch the camera to JPEG (\"Most compatible\") and try again"}
	}

	f, err := os.Open(jpegPath) //nolint:gosec // path produced by transcodeHEIC
	if err != nil {
		_ = os.Remove(jpegPath)
		return nil, fmt.Errorf("opening converted heic: %w", err)
	}

	return f, nil
}

// heifDimensions finds the largest image-spatial-extent ('ispe') property
// in a HEIF file — the primary image, never smaller than its tiles or
// thumbnails — without decoding anything.
func heifDimensions(r io.ReaderAt) (w, h int, ok bool) {
	var best int64
	var visit func(start, end int64, depth int)
	visit = func(start, end int64, depth int) {
		walkBoxes(r, start, end, func(typ string, body, boxEnd int64) {
			switch typ {
			case "meta": // full box: 4 bytes version/flags before children
				if depth == 0 {
					visit(body+4, boxEnd, depth+1)
				}
			case "iprp", "ipco":
				if depth > 0 && depth < 4 {
					visit(body, boxEnd, depth+1)
				}
			case "ispe":
				var b [12]byte
				if boxEnd-body >= 12 {
					if _, err := r.ReadAt(b[:], body); err == nil {
						bw := int64(binary.BigEndian.Uint32(b[4:8]))
						bh := int64(binary.BigEndian.Uint32(b[8:12]))
						if bw*bh > best {
							best, w, h = bw*bh, int(min(bw, 1<<31)), int(min(bh, 1<<31))
						}
					}
				}
			}
		})
	}
	visit(0, 1<<40, 0)

	return w, h, best > 0
}

// walkBoxes iterates ISO-BMFF boxes in [start, end), calling fn with each
// box type and its body range. Stops at the first malformed header.
func walkBoxes(r io.ReaderAt, start, end int64, fn func(typ string, body, boxEnd int64)) {
	for off := start; off+8 <= end; {
		var hdr [16]byte
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		body := off + 8
		switch size {
		case 0: // extends to the end of the enclosing range
			size = end - off
		case 1: // 64-bit size follows
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			body = off + 16
		}
		if size < body-off || off+size > end || size <= 0 {
			return
		}
		fn(typ, body, off+size)
		off += size
	}
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

// imageOrientation reads the EXIF Orientation tag (1..8) for any accepted
// format; 1 when absent or unreadable. Go's decoders ignore it, and the
// re-encode drops the tag, so it must be baked into the pixels or a
// portrait phone photo is stored sideways.
func imageOrientation(r io.ReadSeeker, format string) int {
	var exif []byte
	switch format {
	case "jpeg":
		exif = jpegExif(r)
	case "png":
		exif = pngExif(r)
	case "webp":
		exif = webpExif(r)
	}
	exif = bytes.TrimPrefix(exif, []byte("Exif\x00\x00"))

	return tiffOrientation(exif)
}

// readAtMost reads the first min(n, maxExifBytes) bytes of a metadata
// block. Orientation lives in IFD0 near the start, so a large block (an
// embedded thumbnail, maker notes) is parsed from its bounded prefix
// rather than skipped. JPEG segments never exceed the cap, so the JPEG
// scanner stays aligned on segment boundaries.
func readAtMost(r io.Reader, n int64) []byte {
	if n <= 0 {
		return nil
	}
	b := make([]byte, min(n, maxExifBytes))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil
	}

	return b
}

// jpegExif returns the APP1 Exif payload of a JPEG. Handles the legal
// forms decoders accept: 0xFF fill bytes before a marker and standalone
// markers (TEM, RSTn) with no length.
func jpegExif(r io.ReadSeeker) []byte {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi != [2]byte{0xFF, 0xD8} {
		return nil
	}

	var one [1]byte
	for {
		if _, err := io.ReadFull(r, one[:]); err != nil || one[0] != 0xFF {
			return nil
		}
		marker := byte(0xFF)
		for marker == 0xFF { // fill bytes
			if _, err := io.ReadFull(r, one[:]); err != nil {
				return nil
			}
			marker = one[0]
		}
		switch {
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			continue // standalone, no length
		case marker == 0xDA || marker == 0xD9:
			return nil // image data / end: no EXIF before it
		}

		var lenb [2]byte
		if _, err := io.ReadFull(r, lenb[:]); err != nil {
			return nil
		}
		size := int64(binary.BigEndian.Uint16(lenb[:]))
		if size < 2 {
			return nil
		}
		if marker == 0xE1 {
			seg := readAtMost(r, size-2)
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				return seg
			}
			continue
		}
		if _, err := r.Seek(size-2, io.SeekCurrent); err != nil {
			return nil
		}
	}
}

// pngExif returns the eXIf chunk payload of a PNG (raw TIFF).
func pngExif(r io.ReadSeeker) []byte {
	if _, err := r.Seek(8, io.SeekStart); err != nil { // skip signature
		return nil
	}
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		switch string(hdr[4:8]) {
		case "eXIf":
			return readAtMost(r, size)
		case "IEND":
			return nil
		}
		if _, err := r.Seek(size+4, io.SeekCurrent); err != nil { // data + CRC
			return nil
		}
	}
}

// webpExif returns the EXIF chunk payload of a WebP (RIFF) file.
func webpExif(r io.ReadSeeker) []byte {
	if _, err := r.Seek(12, io.SeekStart); err != nil { // "RIFF" size "WEBP"
		return nil
	}
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil
		}
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		if string(hdr[:4]) == "EXIF" {
			return readAtMost(r, size)
		}
		if _, err := r.Seek(size+size%2, io.SeekCurrent); err != nil { // chunks are padded to even
			return nil
		}
	}
}

// tiffOrientation reads IFD0's Orientation (0x0112) from a TIFF block.
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

	ifd := int64(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > int64(len(t)) {
		return 1
	}
	n := int64(bo.Uint16(t[ifd:]))
	for e := int64(0); e < n; e++ {
		off := ifd + 2 + e*12
		if off+12 > int64(len(t)) {
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
