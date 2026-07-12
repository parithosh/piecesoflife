package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// Per-member, per-kind caps for the issue photo & video dump.
const (
	maxDumpPhotos = 100
	maxDumpVideos = 100
)

// DumpGroup is one member's contribution to the published collage page,
// photos and videos split so the template can lay them out separately.
type DumpGroup struct {
	UserID        int64
	UserName      string
	UserAvatarURL *string
	Photos        []store.DumpItemWithUser
	Videos        []store.DumpItemWithUser
}

// groupDumpItems folds the flat store listing (already ordered by user,
// then upload order) into per-member groups.
func groupDumpItems(items []store.DumpItemWithUser) []DumpGroup {
	groups := make([]DumpGroup, 0)

	for _, item := range items {
		if len(groups) == 0 || groups[len(groups)-1].UserID != item.UserID {
			groups = append(groups, DumpGroup{
				UserID:        item.UserID,
				UserName:      item.UserName,
				UserAvatarURL: item.UserAvatarURL,
			})
		}

		g := &groups[len(groups)-1]
		if item.Kind == "video" {
			g.Videos = append(g.Videos, item)
		} else {
			g.Photos = append(g.Photos, item)
		}
	}

	return groups
}

// handleDumpUpload adds a photo or video to the requesting member's dump for
// an issue. Allowed until the issue is published — the same window in which
// answers stay editable.
// POST /api/issues/{id}/dump
func (s *Server) handleDumpUpload(w http.ResponseWriter, r *http.Request) {
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

	if issue.Status == "published" {
		writeError(w, http.StatusConflict, "issue_published",
			"This issue is already woven & posted — the dump is closed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"Failed to parse form; ensure content-type is multipart/form-data and file is under 1 GB")
		return
	}

	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind == "" {
		kind = "photo"
	}
	if kind != "photo" && kind != "video" {
		writeError(w, http.StatusBadRequest, "invalid_kind",
			"Dump kind must be photo or video")
		return
	}

	limit := maxDumpPhotos
	if kind == "video" {
		limit = maxDumpVideos
	}

	count, err := s.store.CountDumpItemsForUser(ctx, issueID, user.ID, kind)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to count dump items",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to check upload limit")

		return
	}

	if count >= limit {
		writeError(w, http.StatusUnprocessableEntity, "limit_exceeded",
			"Dump limit reached for this issue — remove something first")
		return
	}

	filePath, contentType, ok := s.receiveMediaUpload(w, r, kind)
	if !ok {
		return
	}

	var captionPtr *string
	if caption := strings.TrimSpace(r.FormValue("caption")); caption != "" {
		captionPtr = &caption
	}

	var contentPtr *string
	if kind == "video" {
		contentPtr = &contentType
	}

	itemID, err := s.store.CreateDumpItem(ctx, issueID, user.ID, kind,
		contentPtr, filePath, captionPtr)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create dump item",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to save dump item")

		return
	}

	item, err := s.store.GetDumpItemByID(ctx, itemID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to reload dump item",
			slog.Int64("dump_item_id", itemID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load dump item")

		return
	}

	s.logger.InfoContext(ctx, "Dump item uploaded",
		slog.Int64("issue_id", issueID),
		slog.Int64("dump_item_id", itemID),
		slog.String("kind", kind))

	writeJSON(w, http.StatusCreated, map[string]any{
		"item": item,
		"url":  s.uploadURL(item.FilePath),
	})
}

// handleDumpDelete removes one of the member's own dump items (admins may
// remove anyone's). Blocked once the issue is published.
// DELETE /api/dump/{id}
func (s *Server) handleDumpDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	itemID, ok := s.parseIDParam(w, r, "id", "dump item ID")
	if !ok {
		return
	}

	item, err := s.store.GetDumpItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Dump item not found")
			return
		}

		s.logger.ErrorContext(ctx, "Failed to load dump item",
			slog.Int64("dump_item_id", itemID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	// The Loop check fails closed (requireIssue 500s on lookup errors and
	// 404s cross-Loop) and runs BEFORE the ownership check so probing other
	// Loops' item IDs yields 404, not 403.
	issue, ok := s.requireIssue(w, r, item.IssueID)
	if !ok {
		return
	}

	if item.UserID != user.ID && !isGroupAdmin(r.Context()) {
		writeError(w, http.StatusForbidden, "forbidden", "Not your dump item")
		return
	}

	if issue.Status == "published" {
		writeError(w, http.StatusConflict, "issue_published",
			"This issue is already woven & posted — the dump is closed")
		return
	}

	if _, err := s.store.DeleteDumpItem(ctx, itemID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to delete dump item",
			slog.Int64("dump_item_id", itemID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete dump item")

		return
	}

	// Best-effort file cleanup; the row is authoritative.
	if err := os.Remove(item.FilePath); err != nil && !os.IsNotExist(err) {
		s.logger.WarnContext(ctx, "Failed to remove dump file",
			slog.String("path", item.FilePath),
			slog.String("error", err.Error()))
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleListDumpComments lists comments on a dump photo/video.
// GET /api/dump/{id}/comments
func (s *Server) handleListDumpComments(w http.ResponseWriter, r *http.Request) {
	itemID, ok := s.parseIDParam(w, r, "id", "dump item ID")
	if !ok {
		return
	}

	if _, ok := s.requireDumpItemInGroup(w, r, itemID); !ok {
		return
	}

	comments, err := s.store.ListCommentsByDumpItem(r.Context(), itemID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list dump comments",
			slog.Int64("dump_item_id", itemID),
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

// handleAddDumpComment posts a comment on a dump photo/video. Same rules as
// everywhere else: any member of the Loop, threads one level deep.
// POST /api/dump/{id}/comments
func (s *Server) handleAddDumpComment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	itemID, ok := s.parseIDParam(w, r, "id", "dump item ID")
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

	if _, ok := s.requireDumpItemInGroup(w, r, itemID); !ok {
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
		if err != nil || parent.DumpItemID == nil || *parent.DumpItemID != itemID {
			writeValidationError(w, map[string]string{
				"parent_id": "Parent comment not found on this item",
			})

			return
		}
	}

	id, err := s.store.CreateDumpComment(ctx, user.ID, itemID, req.ParentID, req.Body)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create dump comment",
			slog.Int64("dump_item_id", itemID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to post comment")

		return
	}

	// Queue the contributor's digest mention.
	if item, iErr := s.store.GetDumpItemByID(ctx, itemID); iErr == nil {
		s.enqueueCommentNotification(ctx, item.UserID, user.ID, id)
	}

	created := store.CommentWithUser{
		Comment: store.Comment{
			ID:         id,
			UserID:     user.ID,
			DumpItemID: &itemID,
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

// requireDumpItemInGroup resolves a dump item's owning issue and verifies it
// belongs to the current Loop — same ID-walking guard as the other comment
// targets.
func (s *Server) requireDumpItemInGroup(
	w http.ResponseWriter, r *http.Request, itemID int64,
) (*store.Issue, bool) {
	issue, err := s.store.GetIssueByDumpItemID(r.Context(), itemID)
	if err != nil {
		s.writeNotFound(w, r, "Item not found")
		return nil, false
	}

	if issue.GroupID != currentGroupID(r.Context()) {
		s.writeNotFound(w, r, "Item not found")
		return nil, false
	}

	return issue, true
}
