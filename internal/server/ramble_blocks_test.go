package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockSend runs an authed JSON request with a valid CSRF pair, the way the
// ramble and respond pages call the block APIs. The cookie decides both the
// user and their current Loop.
func blockSend(
	t *testing.T, env *integrationEnv, cookie *http.Cookie,
	method, target, body string,
) *http.Response {
	t.Helper()

	csrfCookie, csrfToken := csrfPair()
	req := newJSONRequest(method, target, body)
	req.AddCookie(cookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfToken)

	return env.do(t, req).Result()
}

// TestRambleBlockEndpoints pins the HTTP-level contract of the journal
// block CRUD: only the owner may touch a block, only text blocks are
// editable, empty text is refused on create, and an emptied block deletes
// itself — taking the day along when it was the last thing on it.
func TestRambleBlockEndpoints(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	meera := env.createUser(t, "Meera", "meera@example.com")
	arun := env.createUser(t, "Arun", "arun@example.com")

	day := time.Now().UTC().Format("2006-01-02")
	blocksTarget := fmt.Sprintf("/api/ramble/%s/blocks", day)

	// Empty text is refused: absence is not a block.
	res := blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPost,
		blocksTarget, `{"text":" \n"}`)
	assert.Equal(t, http.StatusBadRequest, res.StatusCode,
		"whitespace-only create must be refused")

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPost,
		blocksTarget, `{"text":"the gulmohar flowered"}`)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var created struct {
		ID      int64   `json:"id"`
		Content *string `json:"content"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&created))
	require.NotNil(t, created.Content)
	assert.Equal(t, "the gulmohar flowered", *created.Content)

	blockTarget := fmt.Sprintf("/api/ramble/blocks/%d", created.ID)

	// Another member can neither edit nor delete it — uniform 404s, the
	// same shape a probe for a nonexistent ID would see.
	res = blockSend(t, env, env.sessionCookie(t, arun.ID), http.MethodPut,
		blockTarget, `{"text":"hijacked"}`)
	assert.Equal(t, http.StatusNotFound, res.StatusCode,
		"another member's edit must 404")

	res = blockSend(t, env, env.sessionCookie(t, arun.ID), http.MethodDelete,
		blockTarget, "")
	assert.Equal(t, http.StatusNotFound, res.StatusCode,
		"another member's delete must 404")

	// The owner edits in place.
	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		blockTarget, `{"text":"the gulmohar finally flowered"}`)
	require.Equal(t, http.StatusOK, res.StatusCode)

	var updated struct {
		Removed    bool `json:"removed"`
		DayRemoved bool `json:"day_removed"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&updated))
	assert.False(t, updated.Removed)
	assert.False(t, updated.DayRemoved)

	// Media blocks are managed by their own endpoints, never this one.
	rambleID, err := env.store.EnsureRambleDay(ctx, meera.ID, day)
	require.NoError(t, err)

	photoPath := "/up/2026/07/x.jpg"
	photoID, err := env.store.CreateRambleBlock(ctx, rambleID, "photo",
		nil, &photoPath, nil)
	require.NoError(t, err)

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		fmt.Sprintf("/api/ramble/blocks/%d", photoID), `{"text":"sneaky caption"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, res.StatusCode,
		"editing a photo block must be refused")

	// Emptying the text block deletes it; the photo keeps the day alive.
	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		blockTarget, `{"text":""}`)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.NoError(t, json.NewDecoder(res.Body).Decode(&updated))
	assert.True(t, updated.Removed)
	assert.False(t, updated.DayRemoved, "the photo must keep the day alive")

	// On a day holding one lone thought, emptying it removes the day too —
	// trailing whitespace trims to nothing server-side.
	day2 := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPost,
		fmt.Sprintf("/api/ramble/%s/blocks", day2), `{"text":"a lone thought"}`)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var lone struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&lone))

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		fmt.Sprintf("/api/ramble/blocks/%d", lone.ID), `{"text":"\n \n"}`)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.NoError(t, json.NewDecoder(res.Body).Decode(&updated))
	assert.True(t, updated.Removed)
	assert.True(t, updated.DayRemoved, "an emptied last block must take the day")
}

// TestDiaryBlockEndpoints pins the snapshot-side guards: only the section's
// owner may trim a copied block, edits lock once the issue stops
// collecting, and per-block comment threads stay inside the Loop and on
// their block.
func TestDiaryBlockEndpoints(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	meera := env.createUser(t, "Meera", "meera@example.com")
	arun := env.createUser(t, "Arun", "arun@example.com")

	issueID, _ := env.seedIssue(t, "collecting", 7, 2026, 1)

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	for _, txt := range []string{
		"the samosa place changed hands",
		"and the mango cart is back",
		"a stray thought to trim",
	} {
		_, err := env.store.CreateRambleTextBlock(ctx, meera.ID, day, txt)
		require.NoError(t, err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	sectionID, copied, err := env.store.AttachDiarySection(ctx, issueID,
		meera.ID, "", today)
	require.NoError(t, err)
	require.NotZero(t, copied)

	days, err := env.store.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Len(t, days[0].Blocks, 3)

	first := days[0].Blocks[0].ID
	second := days[0].Blocks[1].ID
	third := days[0].Blocks[2].ID
	firstTarget := fmt.Sprintf("/api/diary-blocks/%d", first)

	// Another member cannot rewrite someone's notebook copy.
	res := blockSend(t, env, env.sessionCookie(t, arun.ID), http.MethodPut,
		firstTarget, `{"text":"rewritten"}`)
	assert.Equal(t, http.StatusForbidden, res.StatusCode,
		"another member's snapshot edit must be forbidden")

	// The owner trims one thought; its siblings and the day survive.
	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		fmt.Sprintf("/api/diary-blocks/%d", third), `{"text":""}`)
	require.Equal(t, http.StatusOK, res.StatusCode)

	var trimmedBlock struct {
		Removed    bool `json:"removed"`
		DayRemoved bool `json:"day_removed"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&trimmedBlock))
	assert.True(t, trimmedBlock.Removed)
	assert.False(t, trimmedBlock.DayRemoved)

	// Publishing locks the copy.
	require.NoError(t, env.store.PublishIssue(ctx, issueID))

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPut,
		firstTarget, `{"text":"too late"}`)
	assert.Equal(t, http.StatusConflict, res.StatusCode,
		"snapshot edits must lock after publish")

	// ---- per-block comment threads --------------------------------------
	firstComments := fmt.Sprintf("/api/diary-blocks/%d/comments", first)

	res = blockSend(t, env, env.sessionCookie(t, arun.ID), http.MethodPost,
		firstComments, `{"body":"new owners or new menu?"}`)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var onFirst struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&onFirst))

	res = blockSend(t, env, env.sessionCookie(t, arun.ID), http.MethodPost,
		fmt.Sprintf("/api/diary-blocks/%d/comments", second),
		`{"body":"WHICH mango cart?"}`)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var onSecond struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&onSecond))

	// A reply's parent must live on the same block.
	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPost,
		firstComments,
		fmt.Sprintf(`{"body":"crossed wires","parent_id":%d}`, onSecond.ID))
	assert.Equal(t, http.StatusBadRequest, res.StatusCode,
		"a parent on another block must be refused")

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodPost,
		firstComments,
		fmt.Sprintf(`{"body":"new owners. the chutney is FINE.","parent_id":%d}`,
			onFirst.ID))
	require.Equal(t, http.StatusCreated, res.StatusCode)

	res = blockSend(t, env, env.sessionCookie(t, meera.ID), http.MethodGet,
		firstComments, "")
	require.Equal(t, http.StatusOK, res.StatusCode)

	var listed struct {
		Comments []json.RawMessage `json:"comments"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&listed))
	assert.Len(t, listed.Comments, 2)

	// A member whose current Loop is another one sees uniform 404s.
	loopB := weaveSecondLoop(t, env, "Another Loop")
	zara := env.createUser(t, "Zara", "zara@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, loopB, zara.ID, "member"))
	zaraCookie := env.sessionCookieForGroup(t, zara.ID, loopB)

	res = blockSend(t, env, zaraCookie, http.MethodGet, firstComments, "")
	assert.Equal(t, http.StatusNotFound, res.StatusCode,
		"cross-Loop comment listing must 404")

	res = blockSend(t, env, zaraCookie, http.MethodPost,
		firstComments, `{"body":"peeking"}`)
	assert.Equal(t, http.StatusNotFound, res.StatusCode,
		"cross-Loop comment posting must 404")
}
