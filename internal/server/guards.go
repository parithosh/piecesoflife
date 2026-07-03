package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/parithosh/piecesoflife/internal/store"
)

// loadSettingsOr500 fetches settings for a page handler, writing a plain-text
// 500 on failure. Returns ok=false when the response has already been
// written. API handlers render JSON errors and should keep calling
// GetSettings with writeError instead.
func (s *Server) loadSettingsOr500(
	w http.ResponseWriter, r *http.Request,
) (*store.Settings, bool) {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to load settings",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return nil, false
	}

	return settings, true
}

// parseIDParam reads a path parameter as int64 and writes a 400 on failure.
// Returns (id, true) on success; on failure the response is already written
// and callers should return immediately.
//
// The display name is interpolated into the validation message so errors
// say "Invalid response ID" rather than a generic "Invalid id".
func (s *Server) parseIDParam(
	w http.ResponseWriter, r *http.Request, param, displayName string,
) (int64, bool) {
	id, err := parseID(r, param)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Invalid "+displayName)
		return 0, false
	}

	return id, true
}

// requireIssue fetches an issue by ID, writing a 404 when it doesn't exist
// and a logged 500 on any other store error. Returns ok=false when the
// response has already been written.
func (s *Server) requireIssue(
	w http.ResponseWriter, r *http.Request, issueID int64,
) (*store.Issue, bool) {
	issue, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Issue not found")
			return nil, false
		}

		s.logger.ErrorContext(r.Context(), "Failed to get issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get issue")

		return nil, false
	}

	return issue, true
}

// loadOwnedResponse fetches a response by id and verifies the current user
// owns it. When requireEditable is true it also verifies the parent issue is
// still collecting. On any failure it writes the appropriate error and
// returns ok=false so callers can bail with a single `if !ok { return }`.
//
// Assumes the route is mounted behind authMiddleware — if the caller sits
// on an optional-auth path, use lookupSessionUser + manual checks instead.
func (s *Server) loadOwnedResponse(
	w http.ResponseWriter, r *http.Request, id int64, requireEditable bool,
) (*store.Response, bool) {
	user := UserFromContext(r.Context())

	resp, err := s.store.GetResponseByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Response not found")
		return nil, false
	}

	if resp.UserID != user.ID {
		writeError(w, http.StatusForbidden, "forbidden",
			"Cannot access another user's response")
		return nil, false
	}

	if requireEditable {
		issue, err := s.store.GetIssueByResponseID(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "Issue not found")
			return nil, false
		}

		if issue.Status != "collecting" {
			writeError(w, http.StatusConflict, "not_collecting",
				"This issue is no longer accepting changes")
			return nil, false
		}
	}

	return resp, true
}

// loadOwnedBlock fetches a block, resolves its parent response, and verifies
// the current user owns that response. Behaves identically to
// loadOwnedResponse on the editable check.
func (s *Server) loadOwnedBlock(
	w http.ResponseWriter, r *http.Request, blockID int64, requireEditable bool,
) (*store.ResponseBlock, *store.Response, bool) {
	block, err := s.store.GetBlockByID(r.Context(), blockID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Block not found")
		return nil, nil, false
	}

	resp, ok := s.loadOwnedResponse(w, r, block.ResponseID, requireEditable)
	if !ok {
		return nil, nil, false
	}

	return block, resp, true
}
