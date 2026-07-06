package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

const (
	maxUploadBytes     = 200 << 20 // 200 MB
	maxMultipartMemory = 32 << 20  // 32 MB
	maxMediaBlocks     = 10
	photoBlockType     = "photo"
	audioBlockType     = "audio"
	videoBlockType     = "video"
	maxCommentBytes    = 4000
)

// markdownRenderer renders comment bodies. goldmark's default config strips
// raw HTML (safe by default) — no WithUnsafe() is set.
var markdownRenderer = goldmark.New()

// createResponseRequest is the JSON body for POST /api/responses.
type createResponseRequest struct {
	QuestionID int64 `json:"question_id"`
}

// addBlockRequest is the JSON body for POST /api/responses/{id}/blocks.
type addBlockRequest struct {
	Type      string  `json:"type"`
	Content   *string `json:"content"`
	Caption   *string `json:"caption"`
	LinkURL   *string `json:"link_url"`
	SortOrder int     `json:"sort_order"`
}

// updateBlockRequest is the JSON body for PATCH /api/blocks/{id}.
type updateBlockRequest struct {
	Content *string `json:"content"`
	Caption *string `json:"caption"`
}

// reorderBlocksRequest is the JSON body for POST /api/responses/{id}/blocks/reorder.
type reorderBlocksRequest struct {
	OrderedIDs []int64 `json:"ordered_ids"`
}

// autosaveBlock is a single block entry inside autosaveRequest.
type autosaveBlock struct {
	Type      string  `json:"type"`
	Content   *string `json:"content"`
	FilePath  *string `json:"file_path"`
	Caption   *string `json:"caption"`
	LinkURL   *string `json:"link_url"`
	SortOrder int     `json:"sort_order"`
}

// autosaveRequest is the JSON body for PUT /api/responses/{id}/autosave.
type autosaveRequest struct {
	Version int             `json:"version"`
	Blocks  []autosaveBlock `json:"blocks"`
}

// commentResponse is the wire shape for comments returned from the API:
// the stored comment plus the server-rendered Markdown HTML.
type commentResponse struct {
	store.CommentWithUser

	BodyHTML template.HTML `json:"body_html"`
}

// handleCreateResponse creates a new draft response for a question.
// POST /api/responses
func (s *Server) handleCreateResponse(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	var req createResponseRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if req.QuestionID == 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "question_id is required")
		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), req.QuestionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Question not found for response creation",
			slog.Int64("question_id", req.QuestionID),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusNotFound, "not_found", "Question not found")
		return
	}

	issue, err := s.store.GetIssueByID(r.Context(), question.IssueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Issue not found for response creation",
			slog.Int64("question_id", req.QuestionID),
			slog.Int64("issue_id", question.IssueID),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusNotFound, "not_found", "Issue not found")
		return
	}

	if issue.GroupID != currentGroupID(r.Context()) {
		writeError(w, http.StatusNotFound, "not_found", "Question not found")
		return
	}

	if issue.Status != "collecting" {
		writeError(w, http.StatusConflict, "not_collecting",
			"This issue is not accepting responses")
		return
	}

	// Return existing response if one already exists — idempotent on retry.
	if existing, err := s.store.GetUserResponse(r.Context(), user.ID, req.QuestionID); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	id, err := s.store.CreateResponse(r.Context(), user.ID, req.QuestionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create response",
			slog.Int64("user_id", user.ID),
			slog.Int64("question_id", req.QuestionID),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create response")
		return
	}

	resp, err := s.store.GetResponseByID(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to retrieve newly created response",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve response")
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteResponse deletes an editable response owned by the current user.
// DELETE /api/responses/{id}
func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	if err := s.store.DeleteResponse(r.Context(), id); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete response",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete response")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleSubmitResponse marks a response as submitted.
// POST /api/responses/{id}/submit
func (s *Server) handleSubmitResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	if err := s.store.SubmitResponse(r.Context(), id); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to submit response",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to submit response")
		return
	}

	updated, err := s.store.GetResponseByID(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to retrieve submitted response",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve response")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleListBlocks returns all content blocks for a response.
// GET /api/responses/{id}/blocks
func (s *Server) handleListBlocks(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, false); !ok {
		return
	}

	blocks, err := s.store.ListBlocksByResponse(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list blocks",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list blocks")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks})
}

// handleAddBlock adds a content block to an editable response.
// POST /api/responses/{id}/blocks
func (s *Server) handleAddBlock(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	var req addBlockRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if strings.TrimSpace(req.Type) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "block type is required")
		return
	}
	if req.Type != "text" && req.Type != "link" {
		writeError(w, http.StatusBadRequest, "invalid_block_type",
			"Block type must be text or link")
		return
	}

	blockID, err := s.store.CreateBlock(
		r.Context(), id, req.Type,
		req.Content, nil, req.Caption, req.LinkURL,
		req.SortOrder,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create block",
			slog.Int64("response_id", id),
			slog.String("type", req.Type),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create block")
		return
	}

	block, err := s.store.GetBlockByID(r.Context(), blockID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to retrieve new block",
			slog.Int64("block_id", blockID),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve block")
		return
	}

	writeJSON(w, http.StatusCreated, block)
}

// handleUpdateBlock updates the content and/or caption of an existing block.
// PATCH /api/blocks/{id}
func (s *Server) handleUpdateBlock(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "block ID")
	if !ok {
		return
	}

	if _, _, ok := s.loadOwnedBlock(w, r, id, true); !ok {
		return
	}

	var req updateBlockRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if err := s.store.UpdateBlock(r.Context(), id, req.Content, req.Caption); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update block",
			slog.Int64("block_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update block")
		return
	}

	updated, err := s.store.GetBlockByID(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to retrieve updated block",
			slog.Int64("block_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve block")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteBlock removes a block from an editable response.
// DELETE /api/blocks/{id}
func (s *Server) handleDeleteBlock(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "block ID")
	if !ok {
		return
	}

	if _, _, ok := s.loadOwnedBlock(w, r, id, true); !ok {
		return
	}

	if err := s.store.DeleteBlock(r.Context(), id); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete block",
			slog.Int64("block_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete block")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleReorderBlocks updates the sort order of blocks within a response.
// POST /api/responses/{id}/blocks/reorder
func (s *Server) handleReorderBlocks(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	var req reorderBlocksRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if len(req.OrderedIDs) == 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "ordered_ids must not be empty")
		return
	}

	if err := s.store.ReorderBlocks(r.Context(), id, req.OrderedIDs); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to reorder blocks",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to reorder blocks")
		return
	}

	blocks, err := s.store.ListBlocksByResponse(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list blocks after reorder",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list blocks")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"blocks": blocks})
}

// handleUploadPhoto handles multipart media uploads and creates a media block.
// POST /api/responses/{id}/blocks/upload
// receiveMediaUpload validates and persists the "media" (or legacy "photo")
// form file for the given block type: sniffs and checks the content type,
// writes the file under UPLOAD_PATH/year/month, and returns the stored path
// plus normalized content type. On failure it writes the HTTP error response
// itself and returns ok = false. Callers must have applied MaxBytesReader
// and ParseMultipartForm already.
func (s *Server) receiveMediaUpload(
	w http.ResponseWriter, r *http.Request, blockType string,
) (string, string, bool) {
	file, header, err := r.FormFile("media")
	if err != nil {
		file, header, err = r.FormFile("photo")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Missing upload file in form")
			return "", "", false
		}
	}

	defer file.Close()

	sniff := make([]byte, 512)
	n, readErr := file.Read(sniff)
	if readErr != nil && readErr != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "Failed to read uploaded file")
		return "", "", false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Failed to read uploaded file")
		return "", "", false
	}

	contentType := normalizedUploadContentType(blockType, header.Header.Get("Content-Type"), sniff[:n])
	if !isAllowedUploadType(blockType, contentType) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Unsupported media type for this upload")
		return "", "", false
	}

	ext := extensionFromContentType(contentType)
	now := time.Now()
	year := strconv.Itoa(now.Year())
	month := fmt.Sprintf("%02d", int(now.Month()))

	randomID := auth.GenerateRandomString(8)
	safeName := sanitizeFilename(strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename)))
	filename := fmt.Sprintf("%s_%s%s", randomID, safeName, ext)

	dir := filepath.Join(s.config.UploadPath, year, month)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create upload directory",
			slog.String("dir", dir),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to prepare upload directory")
		return "", "", false
	}

	filePath := filepath.Join(dir, filename)

	dst, err := os.Create(filePath) //nolint:gosec // filePath is constructed from sanitized parts
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create upload file",
			slog.String("path", filePath),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to save uploaded file")
		return "", "", false
	}

	if _, err := io.Copy(dst, file); err != nil {
		_ = dst.Close()
		s.logger.ErrorContext(r.Context(), "Failed to write upload file",
			slog.String("path", filePath),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to write uploaded file")
		return "", "", false
	}

	// The copy must be flushed before ffmpeg reads the file.
	if err := dst.Close(); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to flush upload file",
			slog.String("path", filePath),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to write uploaded file")
		return "", "", false
	}

	s.normalizeWebM(r.Context(), filePath)

	return filePath, contentType, true
}

func (s *Server) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"Failed to parse form; ensure content-type is multipart/form-data and file is under 200 MB")
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

	mediaCount, err := s.store.CountBlocksForResponseType(r.Context(), id, blockType)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to count media blocks",
			slog.Int64("response_id", id),
			slog.String("type", blockType),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to check upload limit")
		return
	}

	if mediaCount >= maxMediaBlocks {
		writeError(w, http.StatusUnprocessableEntity, "limit_exceeded",
			fmt.Sprintf("Maximum of %d %s uploads allowed per response", maxMediaBlocks, blockType))
		return
	}

	filePath, contentType, ok := s.receiveMediaUpload(w, r, blockType)
	if !ok {
		return
	}

	existingBlocks, err := s.store.ListBlocksByResponse(r.Context(), id)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list blocks for sort order",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to determine block order")
		return
	}

	sortOrder := len(existingBlocks)

	caption := r.FormValue("caption")
	var captionPtr *string
	if caption != "" {
		captionPtr = &caption
	}
	var contentPtr *string
	if blockType == audioBlockType || blockType == videoBlockType {
		contentPtr = &contentType
	}

	blockID, err := s.store.CreateBlock(
		r.Context(), id, blockType,
		contentPtr, &filePath, captionPtr, nil,
		sortOrder,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create media block",
			slog.Int64("response_id", id),
			slog.String("type", blockType),
			slog.String("path", filePath),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create media block")
		return
	}

	block, err := s.store.GetBlockByID(r.Context(), blockID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to retrieve media block",
			slog.Int64("block_id", blockID),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to retrieve block")
		return
	}

	s.logger.InfoContext(r.Context(), "Media uploaded",
		slog.Int64("response_id", id),
		slog.Int64("block_id", blockID),
		slog.String("type", blockType),
		slog.String("path", filePath),
	)

	// Embed the block and add the ready-to-use web URL so clients (e.g. the
	// recorder's review step) can preview the file over HTTP without having to
	// reconstruct the /uploads path from the server-side file_path.
	writeJSON(w, http.StatusCreated, struct {
		*store.ResponseBlock
		URL string `json:"url"`
	}{ResponseBlock: block, URL: s.uploadURL(filePath)})
}

// handleAutosave saves blocks for a response with optimistic concurrency control.
// PUT /api/responses/{id}/autosave
func (s *Server) handleAutosave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.loadOwnedResponse(w, r, id, true); !ok {
		return
	}

	var req autosaveRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	// Autosave is for text drafts only — photo/link blocks are managed via
	// the dedicated /blocks endpoints. Rejecting non-text up front prevents
	// a buggy client from sending stale block payloads through the autosave
	// path, and pairs with the store-side guarantee that existing photo and
	// link blocks survive autosaves untouched.
	storeBlocks := make([]store.ResponseBlock, 0, len(req.Blocks))
	for _, b := range req.Blocks {
		if b.Type != "" && b.Type != "text" {
			writeError(w, http.StatusBadRequest, "invalid_block_type",
				"Autosave only accepts text blocks")
			return
		}

		storeBlocks = append(storeBlocks, store.ResponseBlock{
			Type:      "text",
			Content:   b.Content,
			SortOrder: b.SortOrder,
		})
	}

	newVersion, err := s.store.AutosaveResponse(r.Context(), id, req.Version, storeBlocks)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           "version_conflict",
				"current_version": newVersion,
			})
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to autosave response",
			slog.Int64("response_id", id),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to autosave")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"version": newVersion})
}

// handleListComments lists comments on a response.
// GET /api/responses/{id}/comments
func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	responseID, ok := s.parseIDParam(w, r, "id", "response ID")
	if !ok {
		return
	}

	if _, ok := s.requireResponseInGroup(w, r, responseID); !ok {
		return
	}

	comments, err := s.store.ListCommentsByResponse(r.Context(), responseID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list comments",
			slog.Int64("response_id", responseID),
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

// handleAddComment adds a comment to a response. Any logged-in member can
// comment on anyone's response — no ownership requirement beyond the
// response existing.
// POST /api/responses/{id}/comments
func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	responseID, ok := s.parseIDParam(w, r, "id", "response ID")
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

	if _, ok := s.requireResponseInGroup(w, r, responseID); !ok {
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

	resp, err := s.store.GetResponseByID(r.Context(), responseID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Response not found")
		return
	}

	// If parent_id is provided, verify it belongs to the same response
	// (prevents cross-thread injection).
	if req.ParentID != nil {
		parent, err := s.store.GetCommentByID(r.Context(), *req.ParentID)
		if err != nil || parent.ResponseID != responseID {
			writeValidationError(w, map[string]string{
				"parent_id": "Parent comment not found on this response",
			})
			return
		}
	}

	id, err := s.store.CreateComment(
		r.Context(), user.ID, responseID, req.ParentID, req.Body,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create comment",
			slog.Int64("response_id", responseID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to post comment")
		return
	}

	s.notifyCommentAsync(r.Context(), responseID, resp, user, req.Body)

	created := store.CommentWithUser{
		Comment: store.Comment{
			ID:         id,
			UserID:     user.ID,
			ResponseID: responseID,
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

// handleDeleteComment deletes a comment owned by the current user (or any
// comment, if the caller is an admin).
// DELETE /api/comments/{id}
func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())

	id, ok := s.parseIDParam(w, r, "id", "comment ID")
	if !ok {
		return
	}

	comment, err := s.store.GetCommentByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	// The comment must live in the current Loop — isGroupAdmin only vouches
	// for the caller's authority here, not in other Loops.
	if _, ok := s.requireResponseInGroup(w, r, comment.ResponseID); !ok {
		return
	}

	if comment.UserID != user.ID && !isGroupAdmin(r.Context()) {
		writeError(w, http.StatusForbidden, "forbidden", "Cannot delete another user's comment")
		return
	}

	if err := s.store.DeleteComment(r.Context(), id); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete comment",
			slog.Int64("comment_id", id),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete comment")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// notifyCommentAsync kicks off a fire-and-forget comment notification.
// Loads question text for context, then delegates to SendCommentNotification.
func (s *Server) notifyCommentAsync(
	ctx context.Context,
	responseID int64, resp *store.Response, commenter *store.User, body string,
) {
	groupID := currentGroupID(ctx)
	bgCtx := context.WithoutCancel(ctx)

	go func() {
		nCtx, cancel := context.WithTimeout(bgCtx, 45*time.Second)
		defer cancel()

		author, err := s.store.GetUserByID(nCtx, resp.UserID)
		if err != nil {
			s.logger.ErrorContext(nCtx, "Failed to load response author for notification",
				slog.Int64("user_id", resp.UserID),
				slog.String("error", err.Error()))
			return
		}

		question, err := s.store.GetQuestionByID(nCtx, resp.QuestionID)
		questionText := ""
		if err == nil {
			questionText = question.Text
		}

		s.SendCommentNotification(nCtx, groupID, author, commenter, questionText, body, responseID)
	}()
}

// renderCommentBody runs Markdown through goldmark. On error, returns the
// HTML-escaped original body so the comment never vanishes — text is still
// readable, just not formatted.
func renderCommentBody(body string) template.HTML {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(body), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(body))
	}
	return template.HTML(buf.String())
}

// parseID extracts and parses a path value as int64.
func parseID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing path value %q: %w", name, err)
	}

	return id, nil
}

// isAllowedImageType reports whether a MIME type is an accepted image format.
func isAllowedImageType(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func isAllowedAudioType(contentType string) bool {
	switch contentType {
	case "audio/webm", "audio/ogg", "audio/mpeg", "audio/mp4", "audio/wav", "audio/x-wav":
		return true
	default:
		return false
	}
}

func isAllowedVideoType(contentType string) bool {
	switch contentType {
	case "video/webm", "video/mp4", "video/quicktime":
		return true
	default:
		return false
	}
}

func isAllowedUploadType(blockType, contentType string) bool {
	switch blockType {
	case photoBlockType:
		return isAllowedImageType(contentType)
	case audioBlockType:
		return isAllowedAudioType(contentType)
	case videoBlockType:
		return isAllowedVideoType(contentType)
	default:
		return false
	}
}

func normalizedUploadContentType(blockType, headerContentType string, sniff []byte) string {
	detected := http.DetectContentType(sniff)
	if blockType == photoBlockType {
		return detected
	}

	declared := strings.ToLower(strings.TrimSpace(strings.Split(headerContentType, ";")[0]))
	if isAllowedUploadType(blockType, declared) {
		return declared
	}
	if isAllowedUploadType(blockType, detected) {
		return detected
	}

	return declared
}

// extensionFromContentType maps an upload MIME type to a file extension.
func extensionFromContentType(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "audio/webm", "video/webm":
		return ".webm"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	default:
		return ".bin"
	}
}

// sanitizeFilename strips all characters that are not ASCII letters, digits,
// hyphens, or underscores to produce a safe filename component.
func sanitizeFilename(name string) string {
	var b strings.Builder

	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}

	result := b.String()
	if result == "" {
		return "file"
	}

	return result
}
