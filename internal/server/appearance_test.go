package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
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
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("slot", slot))
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

// jpegWithOrientation encodes a w×h JPEG whose left half is red and right
// half blue, with an EXIF APP1 segment carrying the given orientation plus
// a recognisable marker standing in for camera/GPS metadata.
func jpegWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
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
	var enc bytes.Buffer
	require.NoError(t, jpeg.Encode(&enc, img, &jpeg.Options{Quality: 95}))

	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3)
	tiff = binary.LittleEndian.AppendUint32(tiff, 1)
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(orientation))
	tiff = append(tiff, 0, 0, 0, 0, 0, 0, 'G', 'P', 'S', '-', '5', '2', '.', '5', 'N')
	seg := append([]byte("Exif\x00\x00"), tiff...)

	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)

	return append(out, enc.Bytes()[2:]...)
}

// The operator's switch is a ceiling over everything: pages, APIs, and the
// circle's saved choice survives it being turned off and on.
func TestAppearanceFollowsInstanceSwitch(t *testing.T) {
	f := newAppearanceFixture(t)

	assert.Equal(t, http.StatusForbidden, f.upload(t, "photo", jpegWithOrientation(t, 60, 40, 1)).Code,
		"switched off by default")
	assert.Equal(t, http.StatusForbidden,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"fabric":"coastal"}`).Code)

	f.setSwitch(t, true)

	assert.Equal(t, http.StatusForbidden,
		f.call(t, f.member, http.MethodPut, "/api/admin/appearance", `{"fabric":"coastal"}`).Code,
		"members cannot theme the circle")

	up := f.upload(t, "photo", jpegWithOrientation(t, 60, 40, 1))
	require.Equal(t, http.StatusCreated, up.Code, up.Body.String())
	var photo appearancePhotoJSON
	require.NoError(t, json.Unmarshal(up.Body.Bytes(), &photo))

	rr := f.call(t, f.admin, http.MethodPut, "/api/admin/appearance",
		fmt.Sprintf(`{"fabric":"coastal","photo":{"file":%q,"x":30,"y":70}}`, photo.File))
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
		fmt.Sprintf(`{"fabric":"indigo","banner":{"file":%q,"x":50,"y":50}}`, banner.File))
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
			fmt.Sprintf(`{"fabric":"rani","photo":{"file":%q,"x":1,"y":1}}`, file))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "file %q", file)
	}
}

// Restore swaps the published look with the one it replaced, including
// back to (and away from) the house look.
func TestAppearanceRestoreSwapsLooks(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	assert.Equal(t, http.StatusConflict,
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", "").Code,
		"nothing to restore yet")

	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"fabric":"indigo"}`).Code)
	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"fabric":"kilim","main":"#1b4d3e"}`).Code)

	fabricNow := func() *store.Settings {
		s, err := f.env.store.GetSettings(context.Background(), 1)
		require.NoError(t, err)
		return s
	}

	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", "").Code)
	assert.Contains(t, string(fabricNow().Theme), `"indigo"`)
	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", "").Code)
	assert.Contains(t, string(fabricNow().Theme), `"#1b4d3e"`, "restore twice = redo")

	require.Equal(t, http.StatusOK,
		f.call(t, f.admin, http.MethodPut, "/api/admin/appearance", `{"fabric":"rani"}`).Code)
	assert.Nil(t, fabricNow().Theme, "plain Rani is stored as the house look")
	assert.NotContains(t, f.page(t, f.member, f.issuePath), "data-themed")

	require.Equal(t, http.StatusOK, f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/restore", "").Code)
	assert.Contains(t, f.page(t, f.member, f.issuePath), "--rani:#1b4d3e;")
}

// The preview endpoint reports the readability guard's corrections.
func TestAppearancePreviewReportsAdjustments(t *testing.T) {
	f := newAppearanceFixture(t)
	f.setSwitch(t, true)

	rr := f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/preview",
		`{"fabric":"nordic","main":"#fff3a0"}`)
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
		f.call(t, f.admin, http.MethodPost, "/api/admin/appearance/preview", `{"fabric":"tartan"}`).Code)
}
