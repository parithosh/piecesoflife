package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

type appearanceFixture struct {
	env        *integrationEnv
	admin      *http.Cookie
	member     *http.Cookie
	csrfCookie *http.Cookie
	csrfHeader string
	issuePath  string
}

func newAppearanceFixture(t *testing.T) *appearanceFixture {
	t.Helper()
	env := newIntegrationEnv(t)
	admin := env.createUserWithRole(t, "Asha", "asha@example.com", "admin")
	member := env.createUser(t, "Ravi", "ravi@example.com")
	env.seedIssue(t, "published", 6, 2026, 1)
	c, h := csrfPair()

	return &appearanceFixture{
		env: env, admin: env.sessionCookie(t, admin.ID), member: env.sessionCookie(t, member.ID),
		csrfCookie: c, csrfHeader: h, issuePath: "/issues/2026/06",
	}
}

func (f *appearanceFixture) setSwitch(t *testing.T, on bool) {
	t.Helper()
	ctx := context.Background()
	inst, err := f.env.store.GetInstanceSettings(ctx)
	require.NoError(t, err)
	inst.AllowCircleAppearance = on
	require.NoError(t, f.env.store.UpdateInstanceSettings(ctx, inst))
}

func (f *appearanceFixture) call(t *testing.T, session *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newJSONRequest(method, path, body)
	req.AddCookie(session)
	req.AddCookie(f.csrfCookie)
	req.Header.Set("X-CSRF-Token", f.csrfHeader)

	return f.env.do(t, req)
}

func (f *appearanceFixture) page(t *testing.T, session *http.Cookie, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != nil {
		req.AddCookie(session)
	}
	rr := f.env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s", path)

	return rr.Body.String()
}

func (f *appearanceFixture) upload(t *testing.T, slot string, img []byte) *httptest.ResponseRecorder {
	t.Helper()

	return f.uploadFor(t, slot, "1", img)
}

// splitImage is a w×h image whose left half is red and right half blue, so
// a test can tell which way it was rotated.
func splitImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{220, 20, 20, 255}
			if x >= w/2 {
				c = color.RGBA{20, 20, 220, 255}
			}
			img.Set(x, y, c)
		}
	}

	return img
}

// exifTIFF is a minimal TIFF/EXIF block carrying an Orientation tag plus a
// recognisable marker standing in for camera/GPS metadata.
func exifTIFF(orientation int) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3)
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(orientation))

	return append(tiff, 0, 0, 0, 0, 0, 0, 'G', 'P', 'S', '-', '5', '2', '.', '5', 'N')
}

// jpegWithOrientation encodes a split JPEG with an EXIF APP1 segment;
// fill adds legal 0xFF fill bytes before the APP1 marker.
func jpegWithOrientation(t *testing.T, w, h, orientation int, fill ...byte) []byte {
	t.Helper()
	var enc bytes.Buffer
	require.NoError(t, jpeg.Encode(&enc, splitImage(w, h), &jpeg.Options{Quality: 95}))

	seg := append([]byte("Exif\x00\x00"), exifTIFF(orientation)...)
	out := append([]byte{0xFF, 0xD8}, fill...)
	out = append(out, 0xFF, 0xE1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)

	return append(out, enc.Bytes()[2:]...)
}

// pngWithOrientation encodes a split PNG with an eXIf chunk after IHDR.
func pngWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	var enc bytes.Buffer
	require.NoError(t, png.Encode(&enc, splitImage(w, h)))
	raw := enc.Bytes()

	data := exifTIFF(orientation)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	body := append([]byte("eXIf"), data...)
	chunk = append(chunk, body...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(body))

	const ihdrEnd = 8 + 25 // signature + IHDR (len, type, 13 bytes, crc)
	out := append([]byte{}, raw[:ihdrEnd]...)
	out = append(out, chunk...)

	return append(out, raw[ihdrEnd:]...)
}

// heifDeclaring builds a HEIF container (ftyp + meta/iprp/ipco/ispe) that
// declares w×h without any image data.
func heifDeclaring(w, h uint32) []byte {
	box := func(typ string, body []byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
		return append(append(b, typ...), body...)
	}
	ispe := box("ispe", binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32([]byte{0, 0, 0, 0}, w), h))
	meta := box("meta", append([]byte{0, 0, 0, 0}, box("iprp", box("ipco", ispe))...))
	ftyp := box("ftyp", []byte("heic\x00\x00\x00\x00mif1heic"))

	return append(ftyp, meta...)
}

// uploadFor posts a photo claiming it was chosen in editorGroup's editor.
func (f *appearanceFixture) uploadFor(t *testing.T, slot, editorGroup string, img []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("slot", slot))
	require.NoError(t, mw.WriteField("group_id", editorGroup))
	fw, err := mw.CreateFormFile("photo", "family.jpg")
	require.NoError(t, err)
	_, err = fw.Write(img)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/api/admin/appearance/photo", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(f.admin)
	req.AddCookie(f.csrfCookie)
	req.Header.Set("X-CSRF-Token", f.csrfHeader)

	return f.env.do(t, req)
}

// The operator's switch is a ceiling over everything: pages, APIs, and the
// circle's saved choice survives it being turned off and on.
func TestAppearanceFollowsInstanceSwitch(t *testing.T) {
	f := newAppearanceFixture(t)

	assert.Equal(t, http.StatusForbidden, f.upload(t, "photo", jpegWithOrientation(t, 60, 40, 1)).Code,
		"switched off by default")
	assert.Equal(t, http.StatusForbidden,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"group_id":1,"fabric":"coastal"}`).Code)

	f.setSwitch(t, true)

	assert.Equal(t, http.StatusForbidden,
		f.call(t, f.member, http.MethodPut, "/api/admin/appearance", `{"group_id":1,"fabric":"coastal"}`).Code,
		"members cannot theme the circle")

	up := f.upload(t, "photo", jpegWithOrientation(t, 60, 40, 1))
	require.Equal(t, http.StatusCreated, up.Code, up.Body.String())
	var photo appearancePhotoJSON
	require.NoError(t, json.Unmarshal(up.Body.Bytes(), &photo))

	rr := f.call(t, f.admin, http.MethodPut, "/api/admin/appearance",
		fmt.Sprintf(`{"group_id":1,"fabric":"coastal","photo":{"file":%q,"x":30,"y":70}}`, photo.File))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	body := f.page(t, f.member, f.issuePath)
	assert.Contains(t, body, "data-themed")
	assert.Contains(t, body, "--rani:#17435b;")
	assert.Contains(t, body, "data-circle-photo")
	assert.Contains(t, body, photo.URL)
	assert.Contains(t, body, `content="#17435b"`, "phone browser chrome follows the main colour")

	login := f.page(t, nil, "/login")
	assert.NotContains(t, login, "data-themed", "login precedes knowing the circle")

	console := f.page(t, f.admin, "/admin/appearance")
	assert.Contains(t, console, photo.File, "editor starts from the published look")

	f.setSwitch(t, false)

	body = f.page(t, f.member, f.issuePath)
	assert.NotContains(t, body, "data-themed")
	assert.NotContains(t, body, photo.URL)
	assert.NotContains(t, body, "--rani:", "house look emits no theme at all")

	settings, err := f.env.store.GetSettings(context.Background(), 1)
	require.NoError(t, err)
	assert.Contains(t, string(settings.Theme), `"coastal"`, "switching off keeps the circle's choice")

	f.setSwitch(t, true)
	assert.Contains(t, f.page(t, f.member, f.issuePath), "--rani:#17435b;", "and it returns")
}

// Public memento pages carry the circle's colours but never its photos.
func TestAppearanceMementoIsColoursOnly(t *testing.T) {
	f := newAppearanceFixture(t)
	ctx := context.Background()
	f.setSwitch(t, true)
	f.env.setPublicMementos(t, true)

	author := f.env.createUser(t, "Meera", "meera@example.com")
	issueID, qIDs := f.env.seedIssue(t, "collecting", 7, 2026, 1)
	respID, err := f.env.store.CreateResponse(ctx, author.ID, qIDs[0])
	require.NoError(t, err)
	require.NoError(t, f.env.store.SubmitResponse(ctx, respID))
	require.NoError(t, f.env.store.PublishIssue(ctx, issueID))

	up := f.upload(t, "banner", jpegWithOrientation(t, 80, 40, 1))
	require.Equal(t, http.StatusCreated, up.Code, up.Body.String())
	var banner appearancePhotoJSON
	require.NoError(t, json.Unmarshal(up.Body.Bytes(), &banner))
	rr := f.call(t, f.admin, http.MethodPut, "/api/admin/appearance",
		fmt.Sprintf(`{"group_id":1,"fabric":"indigo","banner":{"file":%q,"x":50,"y":50}}`, banner.File))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	memento := f.page(t, nil, fmt.Sprintf("/m/%d", respID))
	assert.Contains(t, memento, "--rani:#243b6b;")
	assert.NotContains(t, memento, banner.File)
	assert.NotContains(t, memento, "data-banner")
}

// Uploaded circle photos are re-encoded: EXIF (and anything riding in it,
// like GPS) is gone, the orientation is baked into the pixels, and large
// images are downsized.
func TestAppearancePhotoIsCleanedAndUpright(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	src := jpegWithOrientation(t, 1200, 800, 6) // stored sideways; displays rotated 90° CW
	require.Contains(t, string(src), "GPS-52.5N")

	up := f.upload(t, "photo", src)
	require.Equal(t, http.StatusCreated, up.Code, up.Body.String())
	var photo appearancePhotoJSON
	require.NoError(t, json.Unmarshal(up.Body.Bytes(), &photo))

	stored, err := os.ReadFile(filepath.Join(f.env.srv.brandingDir(1), photo.File))
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "Exif")
	assert.NotContains(t, string(stored), "GPS-52.5N")

	img, err := jpeg.Decode(bytes.NewReader(stored))
	require.NoError(t, err)
	b := img.Bounds()
	assert.Equal(t, image.Pt(circlePhotoEdge*800/1200, circlePhotoEdge), b.Size(),
		"portrait after rotation, long edge capped")

	// Rotating 90° CW puts the source's left (red) half on top.
	r, _, bl, _ := img.At(b.Dx()/2, b.Dy()/4).RGBA()
	assert.Greater(t, r, bl, "top is red")
	r, _, bl, _ = img.At(b.Dx()/2, b.Dy()*3/4).RGBA()
	assert.Greater(t, bl, r, "bottom is blue")

	assert.Equal(t, http.StatusUnprocessableEntity,
		f.upload(t, "photo", []byte("definitely not an image")).Code)
}

// A published look may only reference photos uploaded into this circle.
func TestAppearanceRejectsForeignPhotoReferences(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	otherDir := f.env.srv.brandingDir(2)
	require.NoError(t, os.MkdirAll(otherDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(otherDir, "0123456789abcdef.jpg"), []byte("x"), 0o644))

	for _, file := range []string{"0123456789abcdef.jpg", "../2/0123456789abcdef.jpg", "fedcba9876543210.jpg"} {
		rr := f.call(t, f.admin, http.MethodPut, "/api/admin/appearance",
			fmt.Sprintf(`{"group_id":1,"fabric":"rani","photo":{"file":%q,"x":1,"y":1}}`, file))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "file %q", file)
	}
}

// Restore swaps the published look with the one it replaced, including
// back to (and away from) the house look.
func TestAppearanceRestoreSwapsLooks(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	assert.Equal(t, http.StatusConflict,
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", `{"group_id":1}`).Code,
		"nothing to restore yet")

	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"group_id":1,"fabric":"indigo"}`).Code)
	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"group_id":1,"fabric":"kilim","main":"#1b4d3e"}`).Code)

	fabricNow := func() *store.Settings {
		s, err := f.env.store.GetSettings(context.Background(), 1)
		require.NoError(t, err)
		return s
	}

	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", `{"group_id":1}`).Code)
	assert.Contains(t, string(fabricNow().Theme), `"indigo"`)
	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", `{"group_id":1}`).Code)
	assert.Contains(t, string(fabricNow().Theme), `"#1b4d3e"`, "restore twice = redo")

	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"group_id":1,"fabric":"rani"}`).Code)
	assert.Nil(t, fabricNow().Theme, "plain Rani is stored as the house look")
	assert.NotContains(t, f.page(t, f.member, f.issuePath), "data-themed")

	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", `{"group_id":1}`).Code)
	assert.Contains(t, f.page(t, f.member, f.issuePath), "--rani:#1b4d3e;")
}

// The preview endpoint reports the readability guard's corrections.
func TestAppearancePreviewReportsAdjustments(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	rr := f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/preview",
		`{"group_id":1,"fabric":"nordic","main":"#fff3a0"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var got struct {
		Declarations string `json:"declarations"`
		Adjustments  []struct {
			Knob, Picked, Used string
		} `json:"adjustments"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got.Adjustments, 1)
	assert.Equal(t, "main", got.Adjustments[0].Knob)
	assert.Equal(t, "#fff3a0", got.Adjustments[0].Picked)
	assert.Contains(t, got.Declarations, "--rani:"+got.Adjustments[0].Used+";")

	assert.Equal(t, http.StatusBadRequest,
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/preview", `{"group_id":1,"fabric":"tartan"}`).Code)
}

func decodeUploaded(t *testing.T, f *appearanceFixture, rr *httptest.ResponseRecorder) (appearancePhotoJSON, image.Image) {
	t.Helper()
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var photo appearancePhotoJSON
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &photo))
	stored, err := os.ReadFile(filepath.Join(f.env.srv.brandingDir(1), photo.File))
	require.NoError(t, err)
	img, err := jpeg.Decode(bytes.NewReader(stored))
	require.NoError(t, err)

	return photo, img
}

// Switching circles in another tab (or inside the preview) must not
// redirect this editor's writes to the newly current circle.
func TestAppearanceRequestsAreBoundToEditorCircle(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	for _, rr := range []*httptest.ResponseRecorder{
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"group_id":2,"fabric":"coastal"}`),
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"fabric":"coastal"}`),
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", `{"group_id":2}`),
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/preview", `{"group_id":2,"fabric":"rani"}`),
		f.call(t, f.admin, http.MethodDelete, "/api/admin/appearance/photo/0123456789abcdef.jpg?group_id=2", ""),
		f.uploadFor(t, "photo", "2", jpegWithOrientation(t, 40, 40, 1)),
	} {
		assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "wrong_circle")
	}

	settings, err := f.env.store.GetSettings(context.Background(), 1)
	require.NoError(t, err)
	assert.Nil(t, settings.Theme)
}

// Unpublished drafts survive another publish (they may be open in another
// tab), die when the editor discards them, and published photos can never
// be deleted through the draft endpoint.
func TestAppearanceDraftPhotoLifecycle(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	published, _ := decodeUploaded(t, f, f.upload(t, "photo", jpegWithOrientation(t, 40, 40, 1)))
	draft, _ := decodeUploaded(t, f, f.upload(t, "banner", jpegWithOrientation(t, 80, 40, 1)))

	rr := f.call(t, f.admin, http.MethodPut, "/api/admin/appearance",
		fmt.Sprintf(`{"group_id":1,"fabric":"indigo","photo":{"file":%q,"x":50,"y":50}}`, published.File))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	exists := func(file string) bool {
		_, err := os.Stat(filepath.Join(f.env.srv.brandingDir(1), file))
		return err == nil
	}
	assert.True(t, exists(draft.File), "a fresh draft is not pruned by someone else's publish")

	del := func(file string) {
		rr := f.call(t, f.admin, http.MethodDelete, "/api/admin/appearance/photo/"+file+"?group_id=1", "")
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	}
	del(draft.File)
	assert.False(t, exists(draft.File), "a discarded draft is deleted")
	del(published.File)
	assert.True(t, exists(published.File), "the published photo is kept")
}

// Stored paths are data, not markup: a tampered or foreign path never
// reaches the page's <style>.
func TestAppearanceIgnoresTamperedPhotoPaths(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)
	ctx := context.Background()

	for _, path := range []string{
		`http://evil.example/");}</style><script>alert(1)</script>/0123456789abcdef.jpg`,
		filepath.Join(f.env.srv.brandingDir(2), "0123456789abcdef.jpg"),
		filepath.Join(f.env.srv.config.UploadPath, "2026", "0123456789abcdef.jpg"),
	} {
		settings, err := f.env.store.GetSettings(ctx, 1)
		require.NoError(t, err)
		raw, err := json.Marshal(map[string]any{
			"v": 1, "fabric": "coastal", "photo": map[string]any{"path": path, "x": 1, "y": 1},
		})
		require.NoError(t, err)
		require.NoError(t, f.env.store.PublishTheme(ctx, 1, settings.Theme, raw))

		body := f.page(t, f.member, f.issuePath)
		assert.Contains(t, body, "--rani:#17435b;", "colours still apply")
		assert.NotContains(t, body, "data-circle-photo", "path %q", path)
		assert.NotContains(t, body, "<script>alert")
		assert.NotContains(t, body, "0123456789abcdef.jpg")
	}
}

// Orientation is honoured for every accepted container, including JPEGs
// with legal fill bytes before their EXIF segment.
func TestAppearancePhotoOrientationAcrossFormats(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	for name, src := range map[string][]byte{
		"png eXIf":         pngWithOrientation(t, 120, 80, 6),
		"jpeg fill bytes":  jpegWithOrientation(t, 120, 80, 6, 0xFF, 0xFF),
		"jpeg plain APP1":  jpegWithOrientation(t, 120, 80, 6),
		"jpeg rotate 180°": jpegWithOrientation(t, 120, 80, 3),
	} {
		_, img := decodeUploaded(t, f, f.upload(t, "photo", src))
		b := img.Bounds()
		r, _, bl, _ := img.At(b.Dx()/2, b.Dy()/4).RGBA()
		if name == "jpeg rotate 180°" {
			assert.Equal(t, image.Pt(120, 80), b.Size(), name)
			r, _, bl, _ = img.At(b.Dx()/4, b.Dy()/2).RGBA()
			assert.Greater(t, bl, r, "%s: left is blue after a half turn", name)
			continue
		}
		assert.Equal(t, image.Pt(80, 120), b.Size(), name)
		assert.Greater(t, r, bl, "%s: top is red after a quarter turn", name)
	}
}

// A HEIC declaring a giant image is refused from its header, before libheif
// is asked to decode it.
func TestAppearanceRejectsOversizedHEICBeforeConverting(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	rr := f.upload(t, "photo", heifDeclaring(20000, 20000))
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Contains(t, rr.Body.String(), "60 megapixels")

	rr = f.upload(t, "photo", heifDeclaring(0, 0)[:40])
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "no readable size → refused")
}
