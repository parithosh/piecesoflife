package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/draw"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Photos are stored at full camera resolution (often 5–10 MB each), which
// made an issue with a few dozen of them crawl on a phone. Pages now offer
// the browser downscaled JPEG copies through srcset, with a tiny blurred
// copy inlined as a placeholder; the original stays one srcset candidate
// and is what the lightbox opens.
//
// Copies are made by one background worker, never inside a request: a page
// whose photo has no copies yet renders the original, exactly as before,
// and queues the photo. So a cold cache, a full queue, or an unwritable
// cache directory can only ever fall back to the old behaviour.
var displaySizes = []int{800, 1200, 1600} // long edges, ascending

const (
	placeholderSize = 24 // long edge of the inlined blurred placeholder

	// variantCacheDir sits inside UPLOAD_PATH so it lives on the uploads
	// volume across redeploys. Nothing references these files, so
	// /uploads/ never serves them directly (CanUserAccessUpload), and the
	// upload integrity walk skips the directory.
	variantCacheDir = ".cache"

	variantQueueSize = 4096
)

// photoImage is what templates need to render a photo.
type photoImage struct {
	URL           string // the original
	Width, Height int    // displayed dimensions; 0 when unknown
	// Placeholder is a data: URI of the blurred copy; empty until the
	// photo's copies exist, and then srcset is offered too.
	Placeholder template.URL
}

// sizes lists the copies that exist for this photo: only sizes smaller
// than the original, and only once the copies have been made.
func (p photoImage) sizes() []int {
	if p.Placeholder == "" {
		return nil
	}
	long := max(p.Width, p.Height)
	var out []int
	for _, size := range displaySizes {
		if size < long {
			out = append(out, size)
		}
	}

	return out
}

// Src is the largest copy, or the original when there are none.
func (p photoImage) Src() string {
	sizes := p.sizes()
	if len(sizes) == 0 {
		return p.URL
	}

	return p.URL + "?w=" + strconv.Itoa(sizes[len(sizes)-1])
}

// Srcset lists the copies and the original with their pixel widths, so the
// browser downloads the smallest file that covers the rendered size, and
// the original whenever no copy is large enough. Empty without copies.
func (p photoImage) Srcset() string {
	sizes := p.sizes()
	if len(sizes) == 0 {
		return ""
	}
	long := max(p.Width, p.Height)
	parts := make([]string, 0, len(sizes)+1)
	for _, size := range sizes {
		parts = append(parts, fmt.Sprintf("%s?w=%d %dw", p.URL, size, max(1, p.Width*size/long)))
	}
	parts = append(parts, fmt.Sprintf("%s %dw", p.URL, p.Width))

	return strings.Join(parts, ", ")
}

// Sizes is the srcset sizes hint for a photo shown at most maxW CSS pixels
// wide and, when maxH > 0, at most maxH tall: full width on screens
// narrower than that, otherwise its capped width.
func (p photoImage) Sizes(maxW, maxH int) string {
	n := maxW
	if p.Width > 0 {
		n = min(n, p.Width) // never shown larger than its own pixels
	}
	if maxH > 0 && p.Height > 0 {
		n = min(n, maxH*p.Width/p.Height)
	}

	return fmt.Sprintf("(max-width: %dpx) 100vw, %dpx", n, n)
}

// CoverSizes is the sizes hint for one of n photos on an archive cover: a
// 4:3 plate, full width on phones and ~320px otherwise, split into one, two
// side-by-side, or four cells. Cells crop (object-fit: cover), so a photo
// wider than its cell is drawn wider than the cell.
func (p photoImage) CoverSizes(n int) string {
	cellW, cellH := 1.0, 0.75 // relative to the plate's width
	switch {
	case n == 2:
		cellW = 0.5
	case n > 2:
		cellW, cellH = 0.5, 0.375
	}
	drawn := cellW
	if p.Height > 0 {
		drawn = max(cellW, cellH*p.Ratio())
	}

	return fmt.Sprintf("(max-width: 720px) %.0fvw, %.0fpx", 100*drawn, 320*drawn)
}

// Ratio is width/height, for CSS that sizes the photo before it loads.
func (p photoImage) Ratio() float64 {
	if p.Height == 0 {
		return 1
	}

	return float64(p.Width) / float64(p.Height)
}

// photoEntry memoises what a render needs to know about one stored photo.
type photoEntry struct {
	modTime     time.Time
	w, h        int
	placeholder template.URL
}

// photoImage is the "photo" template func.
func (s *Server) photoImage(path string) photoImage {
	return s.photoWithURL(path, s.uploadURL(path))
}

// mementoPhoto is the "mementoPhoto" template func: the same photo served
// through the memento's own route, which public viewers can reach.
func (s *Server) mementoPhoto(shareToken, path string) photoImage {
	return s.photoWithURL(path, s.mementoFileURL(shareToken, path))
}

// photoWithURL pairs url with the photo's dimensions, which let the browser
// reserve its space before it loads, and with its copies once they exist.
// A photo without copies is queued for the worker.
func (s *Server) photoWithURL(path, url string) photoImage {
	p := photoImage{URL: url}

	fi, err := os.Stat(path)
	if err != nil {
		return p
	}
	if v, ok := s.photoInfo.Load(path); ok {
		if e := v.(photoEntry); e.modTime.Equal(fi.ModTime()) {
			p.Width, p.Height, p.Placeholder = e.w, e.h, e.placeholder
			if e.placeholder == "" && max(e.w, e.h) > displaySizes[0] {
				s.queueVariants(path)
			}
			return p
		}
	}

	f, err := os.Open(path) //nolint:gosec // path comes from a stored upload row
	if err != nil {
		return p
	}
	defer f.Close()

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return p
	}
	w, h := cfg.Width, cfg.Height
	if _, err := f.Seek(0, 0); err == nil && imageOrientation(f, format) >= 5 {
		w, h = h, w // EXIF orientations 5–8 rotate by 90°
	}

	e := photoEntry{modTime: fi.ModTime(), w: w, h: h}
	if max(w, h) > displaySizes[0] {
		e.placeholder = s.readPlaceholder(path, fi.ModTime())
		if e.placeholder == "" {
			s.queueVariants(path)
		}
	}
	s.photoInfo.Store(path, e)
	p.Width, p.Height, p.Placeholder = e.w, e.h, e.placeholder

	return p
}

// readPlaceholder returns the inlined placeholder when the photo's copies
// are complete (the placeholder is written last) and were made from this
// version of the photo.
func (s *Server) readPlaceholder(path string, modTime time.Time) template.URL {
	file := variantPath(s.variantStem(path), placeholderSize)
	if !madeFrom(file, modTime) {
		return ""
	}
	b, err := os.ReadFile(file) //nolint:gosec // cache path derived from a hash
	if err != nil {
		return ""
	}

	//nolint:gosec // a base64 JPEG we encoded ourselves
	return template.URL("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b))
}

// servableVariant returns the copy of src for the ?w= size when it exists
// and is current; callers serve the original otherwise.
func (s *Server) servableVariant(src, sizeParam string) (string, bool) {
	size, err := strconv.Atoi(sizeParam)
	if err != nil || (size != placeholderSize && !isDisplaySize(size)) {
		return "", false
	}
	orig, err := os.Stat(src)
	if err != nil {
		return "", false
	}
	file := variantPath(s.variantStem(src), size)
	if !madeFrom(file, orig.ModTime()) {
		// If pages offer this copy (say the cache was cleared), the next
		// render re-checks and queues the photo again.
		s.photoInfo.Delete(src)
		return "", false
	}

	return file, true
}

// madeFrom reports whether the cache file exists and was made from the
// version of the photo last modified at modTime: the worker stamps each
// file with its photo's mtime, so a photo replaced or restored from a
// backup (even with an older mtime) never shows the previous copies.
func madeFrom(file string, modTime time.Time) bool {
	fi, err := os.Stat(file)

	return err == nil && fi.ModTime().Equal(modTime)
}

func isDisplaySize(size int) bool {
	for _, v := range displaySizes {
		if v == size {
			return true
		}
	}

	return false
}

// variantStem is the cache path prefix for src's copies. Uploads get
// random names and are never rewritten in place, so the path is the key.
func (s *Server) variantStem(src string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(src)))

	return filepath.Join(filepath.Clean(s.config.UploadPath), variantCacheDir, "images",
		hex.EncodeToString(sum[:12]))
}

func variantPath(stem string, size int) string {
	return stem + "-" + strconv.Itoa(size) + ".jpg"
}

// skipMarker records that a photo gets no copies (small, transparent, or
// undecodable), so it is not decoded again after every restart.
func skipMarker(stem string) string { return stem + "-skip" }

// removeVariants deletes a photo's copies. Callers delete the photo first:
// the worker checks that its photo still exists after writing copies, so
// either the worker or this call removes copies made during a delete.
func (s *Server) removeVariants(path string) {
	s.photoInfo.Delete(path)
	stem := s.variantStem(path)
	for _, size := range append([]int{placeholderSize}, displaySizes...) {
		_ = os.Remove(variantPath(stem, size))
	}
	_ = os.Remove(skipMarker(stem))
}

// variantWorker owns the queue of photos waiting for copies. A single
// goroutine makes them one photo at a time, which bounds both CPU and
// memory no matter how many pages or uploads queue work.
type variantWorker struct {
	once  sync.Once
	queue chan string
	mu    sync.Mutex
	// pending holds photos queued or in progress (zero time) and photos
	// whose copies failed (the failure time), which are retried after
	// variantRetryAfter so an unwritable cache can't keep a core busy.
	pending map[string]time.Time
	wg      sync.WaitGroup

	// The worker holds off until uploads have been quiet for uploadQuiet, so
	// on a small server it never competes with the next photo of a batch.
	uploadQuiet time.Duration
	lastUpload  atomic.Int64 // UnixNano
}

const variantRetryAfter = time.Hour

// queueVariants asks the worker to make path's copies. It never blocks: a
// full queue drops the request, and the next render of the photo asks again.
func (s *Server) queueVariants(path string) {
	v := &s.variants
	v.once.Do(func() {
		v.queue = make(chan string, variantQueueSize)
		v.pending = map[string]time.Time{}
		go s.runVariantWorker()
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	if failed, ok := v.pending[path]; ok && (failed.IsZero() || time.Since(failed) < variantRetryAfter) {
		return
	}
	v.wg.Add(1)
	select {
	case v.queue <- path:
		v.pending[path] = time.Time{}
	default:
		v.wg.Done()
	}
}

// queueUploadVariants queues a just-uploaded photo.
func (s *Server) queueUploadVariants(path string) {
	s.variants.lastUpload.Store(time.Now().UnixNano())
	s.queueVariants(path)
}

func (s *Server) runVariantWorker() {
	v := &s.variants
	for path := range v.queue {
		for {
			wait := time.Until(time.Unix(0, v.lastUpload.Load()).Add(v.uploadQuiet))
			if wait <= 0 {
				break
			}
			time.Sleep(wait)
		}
		err := s.writeVariants(path)
		failed := err != nil && !errors.Is(err, errNoVariants)
		if failed {
			s.logger.Warn("Failed to make photo copies — serving the original",
				slog.String("path", path), slog.String("error", err.Error()))
		}
		// The photo was deleted while its copies were being made.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			s.removeVariants(path)
		}
		// Drop the memo so the next render picks up the copies.
		s.photoInfo.Delete(path)
		v.mu.Lock()
		if failed {
			v.pending[path] = time.Now()
		} else {
			delete(v.pending, path)
		}
		v.mu.Unlock()
		v.wg.Done()
	}
}

// waitForVariants blocks until queued copies are made. Tests use it.
func (s *Server) waitForVariants() { s.variants.wg.Wait() }

var errNoVariants = errors.New("photo gets no copies")

// writeVariants decodes the photo at src and writes each display size
// smaller than it, then the placeholder last, which marks the set complete.
func (s *Server) writeVariants(src string) error {
	stem := s.variantStem(src)
	f, err := os.Open(src) //nolint:gosec // a stored upload path
	if err != nil {
		return fmt.Errorf("opening photo: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading photo info: %w", err)
	}
	mtime := fi.ModTime()
	if madeFrom(skipMarker(stem), mtime) {
		return errNoVariants
	}
	if err := os.MkdirAll(filepath.Dir(stem), 0o750); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}

	cfg, format, err := image.DecodeConfig(f)
	if err != nil || tooManyPixels(cfg.Width, cfg.Height) || max(cfg.Width, cfg.Height) <= displaySizes[0] {
		return skipVariants(stem, mtime)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewinding photo: %w", err)
	}
	// Uploads from before photos were re-encoded may still carry EXIF
	// orientation, which browsers honour on the original; bake it in so the
	// copies match.
	orientation := imageOrientation(f, format)
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewinding photo: %w", err)
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return skipVariants(stem, mtime)
	}
	// A JPEG copy would paint transparent areas white; such photos keep
	// being served as they are.
	if imageHasAlpha(img) {
		return skipVariants(stem, mtime)
	}

	long := max(cfg.Width, cfg.Height)
	base := applyOrientation(downscale(img, min(long, displaySizes[len(displaySizes)-1])), orientation)

	for _, size := range displaySizes {
		if size >= long {
			continue
		}
		if err := writeCacheJPEG(variantPath(stem, size), downscale(base, size), 82, mtime); err != nil {
			return err
		}
	}

	return writeCacheJPEG(variantPath(stem, placeholderSize), downscale(base, placeholderSize), 50, mtime)
}

// writeCacheJPEG writes a copy stamped with its photo's mtime (see madeFrom).
func writeCacheJPEG(dst string, img image.Image, quality int, mtime time.Time) error {
	if err := writeJPEGAtomicQuality(dst, img, quality); err != nil {
		return err
	}

	return os.Chtimes(dst, mtime, mtime)
}

func skipVariants(stem string, mtime time.Time) error {
	if err := os.WriteFile(skipMarker(stem), nil, 0o600); err != nil {
		return fmt.Errorf("writing skip marker: %w", err)
	}
	if err := os.Chtimes(skipMarker(stem), mtime, mtime); err != nil {
		return fmt.Errorf("stamping skip marker: %w", err)
	}

	return errNoVariants
}

// downscale returns img scaled so its long edge is at most maxEdge, by area
// averaging: every output pixel is the mean of the source area it covers,
// so fine detail (fabric, stripes, text) turns into its average tone rather
// than aliasing into moiré. The source is read in strips of rows, so beyond
// the output only a small strip buffer and one float accumulator the size
// of the output are allocated, whatever the photo's resolution.
func downscale(img image.Image, maxEdge int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if long := max(sw, sh); long > maxEdge {
		dw = max(1, int(math.Round(float64(sw)*float64(maxEdge)/float64(long))))
		dh = max(1, int(math.Round(float64(sh)*float64(maxEdge)/float64(long))))
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	if dw == sw && dh == sh {
		draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
		return out
	}

	xs := areaSpans(sw, dw)
	ys := areaSpans(sh, dh)
	acc := make([]float32, dw*dh*4)

	const stripRows = 32
	strip := image.NewRGBA(image.Rect(0, 0, sw, stripRows))
	for y0 := 0; y0 < sh; y0 += stripRows {
		n := min(stripRows, sh-y0)
		draw.Draw(strip, image.Rect(0, 0, sw, n), img, image.Pt(b.Min.X, b.Min.Y+y0), draw.Src)
		for r := range n {
			ys := ys[y0+r]
			row := strip.Pix[r*strip.Stride:]
			for x := range sw {
				xs := xs[x]
				px := row[x*4 : x*4+4]
				for _, yv := range [2]struct {
					o int
					w float32
				}{{ys.o0, ys.w0}, {ys.o0 + 1, ys.w1}} {
					if yv.w == 0 {
						continue
					}
					for _, xv := range [2]struct {
						o int
						w float32
					}{{xs.o0, xs.w0}, {xs.o0 + 1, xs.w1}} {
						if xv.w == 0 {
							continue
						}
						w := yv.w * xv.w
						a := acc[(yv.o*dw+xv.o)*4:]
						a[0] += w * float32(px[0])
						a[1] += w * float32(px[1])
						a[2] += w * float32(px[2])
						a[3] += w * float32(px[3])
					}
				}
			}
		}
	}

	// Each output pixel covers (sw/dw)×(sh/dh) source pixels.
	norm := float32(dw) * float32(dh) / (float32(sw) * float32(sh))
	for i, v := range acc {
		out.Pix[i] = uint8(min(255, v*norm+0.5))
	}

	return out
}

// areaSpan says how one source pixel splits between at most two adjacent
// output pixels, o0 and o0+1, with weights w0 and w1 (summing to 1).
type areaSpan struct {
	o0     int
	w0, w1 float32
}

// areaSpans maps each of n source pixels onto m <= n output pixels.
func areaSpans(n, m int) []areaSpan {
	spans := make([]areaSpan, n)
	for i := range n {
		// Source pixel i covers [i*m, (i+1)*m) in units of 1/n output pixel.
		start, end := i*m, (i+1)*m
		o := start / n
		if boundary := (o + 1) * n; end > boundary && o+1 < m {
			spans[i] = areaSpan{o0: o, w0: float32(boundary-start) / float32(m), w1: float32(end-boundary) / float32(m)}
		} else {
			spans[i] = areaSpan{o0: o, w0: 1}
		}
	}

	return spans
}
