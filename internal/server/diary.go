package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// handleAttachDiary snapshots the caller's journal days since this Loop's
// last published issue into an editable diary section on the issue. The
// copies are theirs to trim before the issue is woven; the journal itself is
// never touched.
// POST /api/issues/{id}/diary
func (s *Server) handleAttachDiary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	if issue.Status != "collecting" {
		writeError(w, http.StatusConflict, "not_collecting",
			"This issue is no longer accepting changes")
		return
	}

	fromDay, throughDay := s.diaryWindow(ctx, issue.GroupID)

	sectionID, copied, err := s.store.AttachDiarySection(
		ctx, issueID, user.ID, fromDay, throughDay,
	)
	if err != nil {
		if errors.Is(err, store.ErrDiaryAttached) {
			writeError(w, http.StatusConflict, "already_attached",
				"Your rambles are already attached to this issue")
			return
		}

		s.logger.ErrorContext(ctx, "Failed to attach diary section",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to attach your rambles")

		return
	}

	s.logger.InfoContext(ctx, "Diary section attached",
		slog.Int64("issue_id", issueID),
		slog.Int64("section_id", sectionID),
		slog.Int("days_copied", copied))

	writeJSON(w, http.StatusCreated, map[string]any{
		"section_id": sectionID,
		"days":       copied,
	})
}

// handleRefreshDiary pulls journal days written since the section was
// attached (or last refreshed) into the snapshot. Explicit only — the
// snapshot never syncs on its own.
// POST /api/issues/{id}/diary/refresh
func (s *Server) handleRefreshDiary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	issue, section, ok := s.loadOwnedDiarySection(w, r, issueID, true)
	if !ok {
		return
	}

	_, throughDay := s.diaryWindow(ctx, issue.GroupID)

	added, err := s.store.RefreshDiarySection(ctx, section.ID, user.ID, throughDay)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to refresh diary section",
			slog.Int64("section_id", section.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to pull in new rambles")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

// handleDetachDiary removes the caller's diary section from an issue.
// Uploads referenced only by the snapshot copies are unlinked.
// DELETE /api/issues/{id}/diary
func (s *Server) handleDetachDiary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	_, section, ok := s.loadOwnedDiarySection(w, r, issueID, true)
	if !ok {
		return
	}

	paths, err := s.store.DeleteDiarySection(ctx, section.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to detach diary section",
			slog.Int64("section_id", section.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to detach your rambles")
		return
	}

	for _, p := range paths {
		s.removeUploadIfUnreferenced(ctx, p)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDiaryDayAutosave replaces one snapshot day's text. Trimming a day to
// nothing removes it from the section.
// PUT /api/diary-days/{id}/autosave
func (s *Server) handleDiaryDayAutosave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dayID, ok := s.parseIDParam(w, r, "id", "day ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedDiaryDay(w, r, dayID, true); !ok {
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
			"text": fmt.Sprintf("A day is capped at %d characters", maxRambleTextBytes),
		})
		return
	}

	blocks := make([]store.DiaryBlock, 0, 1)
	if text := strings.TrimRight(req.Text, "\n "); text != "" {
		blocks = append(blocks, store.DiaryBlock{Type: "text", Content: &text})
	}

	removed, err := s.store.AutosaveDiaryDay(ctx, dayID, blocks)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to autosave diary day",
			slog.Int64("day_id", dayID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to save")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handleDeleteDiaryDay drops one day from the caller's snapshot.
// DELETE /api/diary-days/{id}
func (s *Server) handleDeleteDiaryDay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dayID, ok := s.parseIDParam(w, r, "id", "day ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedDiaryDay(w, r, dayID, true); !ok {
		return
	}

	paths, err := s.store.DeleteDiaryDay(ctx, dayID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to delete diary day",
			slog.Int64("day_id", dayID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to remove the day")
		return
	}

	for _, p := range paths {
		s.removeUploadIfUnreferenced(ctx, p)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteDiaryBlock drops one media block from a snapshot day.
// DELETE /api/diary-blocks/{id}
func (s *Server) handleDeleteDiaryBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	blockID, ok := s.parseIDParam(w, r, "id", "block ID")
	if !ok {
		return
	}

	block, err := s.store.GetDiaryBlockByID(ctx, blockID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Block not found")
		return
	}

	if _, ok := s.loadOwnedDiaryDay(w, r, block.DiaryDayID, true); !ok {
		return
	}

	if err := s.store.DeleteDiaryBlock(ctx, blockID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to delete diary block",
			slog.Int64("block_id", blockID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete block")
		return
	}

	if block.FilePath != nil {
		s.removeUploadIfUnreferenced(ctx, *block.FilePath)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListDiaryComments lists comments on a published notebook day.
// GET /api/diary-days/{id}/comments
func (s *Server) handleListDiaryComments(w http.ResponseWriter, r *http.Request) {
	dayID, ok := s.parseIDParam(w, r, "id", "day ID")
	if !ok {
		return
	}

	if _, ok := s.requireDiaryDayInGroup(w, r, dayID); !ok {
		return
	}

	comments, err := s.store.ListCommentsByDiaryDay(r.Context(), dayID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list diary comments",
			slog.Int64("day_id", dayID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list comments")
		return
	}

	out := make([]commentResponse, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentResponse{
			CommentWithUser: c,
			BodyHTML:        renderCommentBody(c.Body),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"comments": out})
}

// handleAddDiaryComment posts a comment on a notebook day. Any member of the
// Loop can comment; threads stay one level deep like response comments.
// POST /api/diary-days/{id}/comments
func (s *Server) handleAddDiaryComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	dayID, ok := s.parseIDParam(w, r, "id", "day ID")
	if !ok {
		return
	}

	var req struct {
		Body     string `json:"body"`
		ParentID *int64 `json:"parent_id"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if _, ok := s.requireDiaryDayInGroup(w, r, dayID); !ok {
		return
	}

	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		writeValidationError(w, map[string]string{"body": "Comment body is required"})
		return
	}

	if len(req.Body) > maxCommentBytes {
		writeValidationError(w, map[string]string{
			"body": fmt.Sprintf("Comment too long (max %d characters)", maxCommentBytes),
		})
		return
	}

	if req.ParentID != nil {
		parent, err := s.store.GetCommentByID(ctx, *req.ParentID)
		if err != nil || parent.DiaryDayID == nil || *parent.DiaryDayID != dayID {
			writeValidationError(w, map[string]string{
				"parent_id": "Parent comment not found on this day",
			})
			return
		}
	}

	id, err := s.store.CreateDiaryComment(ctx, user.ID, dayID, req.ParentID, req.Body)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create diary comment",
			slog.Int64("day_id", dayID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to post comment")
		return
	}

	// Queue the notebook owner's digest mention.
	if day, dErr := s.store.GetDiaryDayByID(ctx, dayID); dErr == nil {
		if section, sErr := s.store.GetDiarySectionByID(ctx, day.SectionID); sErr == nil {
			s.enqueueCommentNotifications(ctx, section.UserID, user.ID, id, req.ParentID)
		}
	}

	created := store.CommentWithUser{
		Comment: store.Comment{
			ID:         id,
			UserID:     user.ID,
			DiaryDayID: &dayID,
			ParentID:   req.ParentID,
			Body:       req.Body,
			CreatedAt:  time.Now(),
		},
		AuthorName:      user.Name,
		AuthorAvatarURL: user.AvatarURL,
	}

	writeJSON(w, http.StatusCreated, commentResponse{
		CommentWithUser: created,
		BodyHTML:        renderCommentBody(req.Body),
	})
}

// requireDiaryDayInGroup resolves a notebook day's owning issue and verifies
// it belongs to the current Loop — same ID-walking guard as
// requireResponseInGroup.
func (s *Server) requireDiaryDayInGroup(
	w http.ResponseWriter, r *http.Request, dayID int64,
) (*store.Issue, bool) {
	issue, err := s.store.GetIssueByDiaryDayID(r.Context(), dayID)
	if err != nil {
		s.writeNotFound(w, r, "Day not found")
		return nil, false
	}

	if issue.GroupID != currentGroupID(r.Context()) {
		s.writeNotFound(w, r, "Day not found")
		return nil, false
	}

	return issue, true
}

// diaryWindow computes the journal window offered to a diary section on this
// Loop: from the last published issue's publish date (inclusive — nothing
// falls in a crack; duplicates are the member's to trim) through today, both
// in the Loop's timezone. fromDay is empty when nothing has been published,
// which CountRambleDaysBetween and the copy treat as "from the beginning".
func (s *Server) diaryWindow(
	ctx context.Context, groupID int64,
) (fromDay, throughDay string) {
	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		settings = nil
	}

	loc := s.settingsLocation(ctx, settings)
	throughDay = time.Now().In(loc).Format("2006-01-02")

	last, err := s.store.GetLatestPublishedIssue(ctx, groupID)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to find last published issue for diary window",
			slog.Int64("group_id", groupID),
			slog.String("error", err.Error()))
		return "", throughDay
	}

	if last != nil && last.PublishedAt != nil {
		fromDay = last.PublishedAt.In(loc).Format("2006-01-02")
	}

	return fromDay, throughDay
}

// loadOwnedDiarySection fetches the caller's section on an issue, verifying
// Loop membership and (optionally) that the issue is still collecting.
func (s *Server) loadOwnedDiarySection(
	w http.ResponseWriter, r *http.Request, issueID int64, requireEditable bool,
) (*store.Issue, *store.DiarySection, bool) {
	user := UserFromContext(r.Context())

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return nil, nil, false
	}

	if requireEditable && issue.Status != "collecting" {
		writeError(w, http.StatusConflict, "not_collecting",
			"This issue is no longer accepting changes")
		return nil, nil, false
	}

	section, err := s.store.GetDiarySection(r.Context(), issueID, user.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"Your rambles aren't attached to this issue")
		return nil, nil, false
	}

	return issue, section, true
}

// loadOwnedDiaryDay fetches a snapshot day and verifies the current user
// owns its section; requireEditable additionally demands a collecting issue.
func (s *Server) loadOwnedDiaryDay(
	w http.ResponseWriter, r *http.Request, dayID int64, requireEditable bool,
) (*store.DiaryDay, bool) {
	user := UserFromContext(r.Context())

	day, err := s.store.GetDiaryDayByID(r.Context(), dayID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Day not found")
		return nil, false
	}

	section, err := s.store.GetDiarySectionByID(r.Context(), day.SectionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Day not found")
		return nil, false
	}

	if section.UserID != user.ID {
		writeError(w, http.StatusForbidden, "forbidden",
			"Cannot edit another member's notebook")
		return nil, false
	}

	issue, err := s.store.GetIssueByID(r.Context(), section.IssueID)
	if err != nil || issue.GroupID != currentGroupID(r.Context()) {
		writeError(w, http.StatusNotFound, "not_found", "Day not found")
		return nil, false
	}

	if requireEditable && issue.Status != "collecting" {
		writeError(w, http.StatusConflict, "not_collecting",
			"This issue is no longer accepting changes")
		return nil, false
	}

	return day, true
}
