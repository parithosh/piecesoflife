package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// maxRambleTextBytes caps one day's autosaved text — same spirit as the
// comment cap, sized for a long journal page rather than a note.
const maxRambleTextBytes = 64_000

// rambleDayPattern matches the 'YYYY-MM-DD' day labels journal pages are
// keyed by.
var rambleDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// RamblePageData is the template data for the private journal page.
type RamblePageData struct {
	PageData

	// Today is the server's date guess ('YYYY-MM-DD'); the page script
	// re-derives it from the browser clock, since "the day you opened the
	// page" is the client's day, not the server's.
	Today    string
	DayCount int
	Entries  []RambleTimelineEntry
}

// RambleTimelineEntry is one row of the journal timeline: either a day the
// member wrote, or an "issue published here" divider from one of their
// Loops.
type RambleTimelineEntry struct {
	Kind string // "day" | "issue"

	// Day fields (Kind == "day").
	Day        *store.RambleDay
	DayDisplay string // e.g. "Tuesday 23 June 2026"

	// Issue divider fields (Kind == "issue").
	LoopName     string
	IssueLabel   string // e.g. "June 2026"
	PublishedDay string // display, day-first

	sortKey string
}

// handleRamblePage renders the private journal.
// GET /ramble
func (s *Server) handleRamblePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	days, err := s.store.ListRambleDays(ctx, user.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list ramble days",
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	entries := make([]RambleTimelineEntry, 0, len(days)+8)

	for i := range days {
		entries = append(entries, RambleTimelineEntry{
			Kind:       "day",
			Day:        &days[i],
			DayDisplay: rambleDayDisplay(days[i].Ramble.Day),
			sortKey:    days[i].Ramble.Day,
		})
	}

	entries = append(entries, s.rambleIssueDividers(r, user.ID)...)

	// Newest first; a publish divider dated the same as a journal day sits
	// below that day (the day belongs to the window that opened at publish).
	sortRambleTimeline(entries)

	data := RamblePageData{
		PageData: s.newPageData(r),
		Today:    time.Now().Format("2006-01-02"),
		DayCount: len(days),
		Entries:  entries,
	}

	s.renderPage(w, "ramble.html", data)
}

// handleGetRambleDay returns one journal day with blocks — the page script
// hydrates the "today" editor with it after deriving the browser-local date.
// GET /api/ramble/{day}
func (s *Server) handleGetRambleDay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	day, ok := s.requireRambleDayParam(w, r)
	if !ok {
		return
	}

	ramble, err := s.store.GetRambleByDay(ctx, user.ID, day)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ramble": nil, "blocks": []any{}})
		return
	}

	blocks, err := s.store.ListRambleBlocks(ctx, ramble.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list ramble blocks",
			slog.Int64("ramble_id", ramble.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load the day")
		return
	}

	out := make([]rambleBlockResponse, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, s.rambleBlockJSON(b))
	}

	writeJSON(w, http.StatusOK, map[string]any{"ramble": ramble, "blocks": out})
}

// handleRambleAutosave replaces one day's text. An empty save deletes the
// day when no media remains — empty days are invisible.
// PUT /api/ramble/{day}/autosave
func (s *Server) handleRambleAutosave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	day, ok := s.requireRambleDayParam(w, r)
	if !ok {
		return
	}

	var req struct {
		Text string `json:"text"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if len(req.Text) > maxRambleTextBytes {
		writeValidationError(w, map[string]string{
			"text": fmt.Sprintf("A day's ramble is capped at %d characters", maxRambleTextBytes),
		})
		return
	}

	blocks := make([]store.RambleBlock, 0, 1)
	if text := strings.TrimRight(req.Text, "\n "); text != "" {
		blocks = append(blocks, store.RambleBlock{Type: "text", Content: &text})
	}

	rambleID, removed, err := s.store.AutosaveRambleDay(ctx, user.ID, day, blocks)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to autosave ramble",
			slog.Int64("user_id", user.ID),
			slog.String("day", day),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to save")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ramble_id": rambleID,
		"removed":   removed,
	})
}

// handleRambleMediaUpload attaches a photo/audio/video to one journal day.
// POST /api/ramble/{day}/media
func (s *Server) handleRambleMediaUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	day, ok := s.requireRambleDayParam(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"Failed to parse form; ensure content-type is multipart/form-data and file is under 1 GB")
		return
	}

	blockType := strings.TrimSpace(r.FormValue("kind"))
	if blockType == "" {
		blockType = photoBlockType
	}
	if blockType != photoBlockType && blockType != audioBlockType && blockType != videoBlockType {
		writeError(w, http.StatusBadRequest, "invalid_block_type",
			"Upload kind must be photo, audio, or video")
		return
	}

	rambleID, err := s.store.EnsureRambleDay(ctx, user.ID, day)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to ensure ramble day",
			slog.Int64("user_id", user.ID),
			slog.String("day", day),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to prepare the day")
		return
	}

	mediaCount, err := s.store.CountRambleBlocksForType(ctx, rambleID, blockType)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to count ramble media blocks",
			slog.Int64("ramble_id", rambleID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to check upload limit")
		return
	}

	if mediaCount >= maxMediaBlocks {
		writeError(w, http.StatusUnprocessableEntity, "limit_exceeded",
			fmt.Sprintf("Maximum of %d %s uploads per day", maxMediaBlocks, blockType))
		return
	}

	filePath, contentType, ok := s.receiveMediaUpload(w, r, blockType)
	if !ok {
		return
	}

	caption := r.FormValue("caption")
	var captionPtr *string
	if caption != "" {
		captionPtr = &caption
	}
	var contentPtr *string
	if blockType == audioBlockType || blockType == videoBlockType {
		contentPtr = &contentType
	}

	blockID, err := s.store.CreateRambleBlock(
		ctx, rambleID, blockType, contentPtr, &filePath, captionPtr,
	)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create ramble media block",
			slog.Int64("ramble_id", rambleID),
			slog.String("type", blockType),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create media block")
		return
	}

	block, err := s.store.GetRambleBlockByID(ctx, blockID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to retrieve ramble block",
			slog.Int64("block_id", blockID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve block")
		return
	}

	writeJSON(w, http.StatusCreated, s.rambleBlockJSON(*block))
}

// handleDeleteRambleBlock removes a journal media block. The uploaded file
// is unlinked only when no diary snapshot still references it.
// DELETE /api/ramble/blocks/{id}
func (s *Server) handleDeleteRambleBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	id, ok := s.parseIDParam(w, r, "id", "block ID")
	if !ok {
		return
	}

	block, err := s.store.GetRambleBlockByID(ctx, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Block not found")
		return
	}

	ramble, err := s.store.GetRambleByID(ctx, block.RambleID)
	if err != nil || ramble.UserID != user.ID {
		writeError(w, http.StatusNotFound, "not_found", "Block not found")
		return
	}

	if err := s.store.DeleteRambleBlock(ctx, id); err != nil {
		s.logger.ErrorContext(ctx, "Failed to delete ramble block",
			slog.Int64("block_id", id),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete block")
		return
	}

	if block.FilePath != nil {
		s.removeUploadIfUnreferenced(ctx, *block.FilePath)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRambleExport streams the member's full journal as JSON — the
// person-scoped counterpart of the Loop export. Never included in Loop
// exports: the journal is private.
// GET /api/me/ramble/export
func (s *Server) handleRambleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	days, err := s.store.ListRambleDays(ctx, user.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to build ramble export",
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to build export")
		return
	}

	type exportRambleDay struct {
		Ramble store.Ramble          `json:"ramble"`
		Blocks []rambleBlockResponse `json:"blocks"`
	}

	out := make([]exportRambleDay, 0, len(days))
	for _, d := range days {
		blocks := make([]rambleBlockResponse, 0, len(d.Blocks))
		for _, b := range d.Blocks {
			blocks = append(blocks, s.rambleBlockJSON(b))
		}

		out = append(out, exportRambleDay{Ramble: d.Ramble, Blocks: blocks})
	}

	filename := fmt.Sprintf("ramble-export-%s.json",
		time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filename))

	writeJSON(w, http.StatusOK, map[string]any{
		"version":     1,
		"exported_at": time.Now().UTC(),
		"user_id":     user.ID,
		"days":        out,
	})
}

// rambleBlockResponse embeds a block with its browser-accessible URL.
type rambleBlockResponse struct {
	store.RambleBlock

	URL string `json:"url,omitempty"`
}

func (s *Server) rambleBlockJSON(b store.RambleBlock) rambleBlockResponse {
	url := ""
	if b.FilePath != nil {
		url = s.uploadURL(*b.FilePath)
	}

	return rambleBlockResponse{RambleBlock: b, URL: url}
}

// requireRambleDayParam validates the {day} path value: shape, parseability,
// and a loose range (nothing before 2000, nothing more than two days ahead —
// enough slack for any client timezone without opening arbitrary future
// pages).
func (s *Server) requireRambleDayParam(
	w http.ResponseWriter, r *http.Request,
) (string, bool) {
	day := r.PathValue("day")
	if !rambleDayPattern.MatchString(day) {
		writeError(w, http.StatusBadRequest, "invalid_day", "Invalid day")
		return "", false
	}

	parsed, err := time.Parse("2006-01-02", day)
	if err != nil || parsed.Year() < 2000 {
		writeError(w, http.StatusBadRequest, "invalid_day", "Invalid day")
		return "", false
	}

	if parsed.After(time.Now().AddDate(0, 0, 2)) {
		writeError(w, http.StatusBadRequest, "invalid_day",
			"That day hasn't happened yet")
		return "", false
	}

	return day, true
}

// removeUploadIfUnreferenced unlinks an uploaded file once nothing in the
// journal or any diary snapshot points at it. Best-effort like other upload
// removals — a leftover file only costs space.
func (s *Server) removeUploadIfUnreferenced(ctx context.Context, path string) {
	if path == "" {
		return
	}

	refs, err := s.store.CountUploadsReferencing(ctx, path)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to count upload references",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return
	}

	if refs > 0 {
		return
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.WarnContext(ctx, "Failed to remove unreferenced upload",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
}

// rambleIssueDividers builds "issue published here" timeline markers from
// every Loop the member belongs to, dated in each Loop's own timezone.
func (s *Server) rambleIssueDividers(
	r *http.Request, userID int64,
) []RambleTimelineEntry {
	ctx := r.Context()

	groups, err := s.store.ListUserGroups(ctx, userID)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to list groups for ramble dividers",
			slog.String("error", err.Error()))
		return nil
	}

	published := "published"
	entries := make([]RambleTimelineEntry, 0, len(groups)*4)

	for _, g := range groups {
		issues, err := s.store.ListIssues(ctx, g.GroupID, &published)
		if err != nil {
			s.logger.WarnContext(ctx, "Failed to list issues for ramble dividers",
				slog.Int64("group_id", g.GroupID),
				slog.String("error", err.Error()))
			continue
		}

		settings, err := s.store.GetSettings(ctx, g.GroupID)
		if err != nil {
			settings = nil
		}

		loc := s.settingsLocation(ctx, settings)

		for i := range issues {
			if issues[i].PublishedAt == nil {
				continue
			}

			local := issues[i].PublishedAt.In(loc)
			label := issues[i].OpensAt.In(loc).Format("January 2006")
			if issues[i].Title != nil && *issues[i].Title != "" {
				label = *issues[i].Title
			}

			entries = append(entries, RambleTimelineEntry{
				Kind:         "issue",
				LoopName:     g.LoopName,
				IssueLabel:   label,
				PublishedDay: local.Format("Monday 2 January 2006"),
				sortKey:      local.Format("2006-01-02"),
			})
		}
	}

	return entries
}

// rambleDayDisplay renders a 'YYYY-MM-DD' label day-first for the page.
func rambleDayDisplay(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}

	return t.Format("Monday 2 January 2006")
}

// sortRambleTimeline orders entries newest-first. On a date shared by a
// journal day and a publish divider, the day sorts first (above the
// divider): a day written on publish day belongs to the next window.
func sortRambleTimeline(entries []RambleTimelineEntry) {
	slices.SortStableFunc(entries, func(a, b RambleTimelineEntry) int {
		switch {
		case a.sortKey > b.sortKey:
			return -1
		case a.sortKey < b.sortKey:
			return 1
		case a.Kind == "day" && b.Kind == "issue":
			return -1
		case a.Kind == "issue" && b.Kind == "day":
			return 1
		default:
			return 0
		}
	})
}
