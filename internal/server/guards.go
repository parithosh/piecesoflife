package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/parithosh/piecesoflife/internal/store"
)

// loadSettingsOr500 returns the current Loop's settings, resolved by
// authMiddleware into the request context. Writes a plain-text 500 (and
// returns ok=false) if no Loop is resolved — group-scoped page handlers sit
// behind requireGroupMiddleware, so that indicates a wiring bug.
func (s *Server) loadSettingsOr500(
	w http.ResponseWriter, r *http.Request,
) (*store.Settings, bool) {
	gc := GroupFromContext(r.Context())
	if gc == nil {
		s.logger.ErrorContext(r.Context(),
			"No group context on a group-scoped page")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return nil, false
	}

	return gc.Settings, true
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
//
// Issues outside the current Loop 404 — except on GET when the user can
// enter the owning Loop, in which case the request transparently switches
// their current Loop and redirects to the same URL. That keeps old links,
// bookmarks, and cross-Loop email links working without group-prefixed
// routes.
func (s *Server) requireIssue(
	w http.ResponseWriter, r *http.Request, issueID int64,
) (*store.Issue, bool) {
	issue, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeNotFound(w, r, "Issue not found")
			return nil, false
		}

		s.logger.ErrorContext(r.Context(), "Failed to get issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get issue")

		return nil, false
	}

	if issue.GroupID != currentGroupID(r.Context()) {
		user := UserFromContext(r.Context())

		if r.Method == http.MethodGet && user != nil &&
			s.tryGroup(r.Context(), user, issue.GroupID) != nil {
			s.switchGroup(w, r, user, issue.GroupID)
			return nil, false
		}

		s.writeNotFound(w, r, "Issue not found")

		return nil, false
	}

	return issue, true
}

// requireResponseInGroup resolves a response's owning issue and verifies it
// belongs to the current Loop. Used by handlers that take a bare response
// ID from the client (comments, listings) so IDs can't be walked across
// Loops. No auto-switch here — these are fetch endpoints.
func (s *Server) requireResponseInGroup(
	w http.ResponseWriter, r *http.Request, responseID int64,
) (*store.Issue, bool) {
	issue, err := s.store.GetIssueByResponseID(r.Context(), responseID)
	if err != nil {
		s.writeNotFound(w, r, "Response not found")
		return nil, false
	}

	if issue.GroupID != currentGroupID(r.Context()) {
		s.writeNotFound(w, r, "Response not found")
		return nil, false
	}

	return issue, true
}

// writeNotFound renders a 404 in the shape the caller expects: a JSON error
// for API routes, a plain 404 for pages.
func (s *Server) writeNotFound(w http.ResponseWriter, r *http.Request, msg string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not_found", msg)
		return
	}

	http.NotFound(w, r)
}

// switchGroup makes groupID the user's current Loop (session + last used)
// and replays the request URL so it re-runs under the new Loop. Callers
// must have validated access via tryGroup first.
func (s *Server) switchGroup(
	w http.ResponseWriter, r *http.Request, user *store.User, groupID int64,
) {
	gc := GroupFromContext(r.Context())
	if gc != nil && gc.SessionID != 0 {
		if err := s.store.SetSessionGroup(r.Context(), gc.SessionID, groupID); err != nil {
			s.logger.ErrorContext(r.Context(), "Failed to switch session group",
				slog.String("error", err.Error()))
		}
	}

	if err := s.store.SetLastGroup(r.Context(), user.ID, groupID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to persist last group",
			slog.String("error", err.Error()))
	}

	// Drop any explicit ?g= from the replayed URL: it outranks the session
	// group during resolution, so a stale value would fight the switch we
	// just made and redirect forever.
	q := r.URL.Query()
	q.Del("g")
	r.URL.RawQuery = q.Encode()

	http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
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

// isGroupAdminOver reports whether the request's user administers the
// current Loop AND the target user belongs to it — the scope of an admin's
// authority over someone else's profile or preferences.
func (s *Server) isGroupAdminOver(ctx context.Context, targetUserID int64) bool {
	if !isGroupAdmin(ctx) {
		return false
	}

	gc := GroupFromContext(ctx)
	if gc == nil {
		return false
	}

	_, err := s.store.GetMembership(ctx, gc.Group.ID, targetUserID)

	return err == nil
}
