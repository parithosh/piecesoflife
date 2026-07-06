package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// LoopCard is one Loop on the /loops page: the membership plus the state of
// its current round.
type LoopCard struct {
	store.UserGroup
	Current bool
	// Collecting is true while the Loop has an open round; Deadline is that
	// round's deadline.
	Collecting bool
	Deadline   time.Time
	// LatestEdition is the newest published edition's label ("July 2026"),
	// empty when nothing has been published yet.
	LatestEdition string
}

// LoopsPageData is the template data for the "Your Loops" page.
type LoopsPageData struct {
	PageData
	Cards []LoopCard
}

// InstancePageData is the template data for the operator console.
type InstancePageData struct {
	PageData
	Instance *store.InstanceSettings
	Groups   []GroupRow
}

// GroupRow is one Loop in the operator console list.
type GroupRow struct {
	store.GroupOverview
	Collecting bool
	Deadline   time.Time
}

// handleLoopsPage renders the "Your Loops" chooser.
// GET /loops
func (s *Server) handleLoopsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pd := s.newPageData(r)

	cards := make([]LoopCard, 0, len(pd.Loops))

	for _, ug := range pd.Loops {
		card := LoopCard{UserGroup: ug}

		if pd.Group != nil && pd.Group.ID == ug.GroupID {
			card.Current = true
		}

		if issue, err := s.store.GetActiveIssue(ctx, ug.GroupID); err == nil &&
			issue.Status == "collecting" {
			card.Collecting = true
			card.Deadline = issue.Deadline
		}

		published := "published"
		if issues, err := s.store.ListIssues(ctx, ug.GroupID, &published); err == nil &&
			len(issues) > 0 {
			card.LatestEdition = formatDate(issues[0].OpensAt)
		}

		cards = append(cards, card)
	}

	s.renderPage(w, "loops.html", LoopsPageData{PageData: pd, Cards: cards})
}

// handleSwitchGroup makes another Loop the session's current one.
// POST /api/me/group
func (s *Server) handleSwitchGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	var req struct {
		GroupID int64 `json:"group_id"`
	}

	if err := readJSON(r, &req); err != nil || req.GroupID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if s.tryGroup(ctx, user, req.GroupID) == nil {
		writeError(w, http.StatusNotFound, "not_found", "No such Loop")
		return
	}

	if gc := GroupFromContext(ctx); gc != nil && gc.SessionID != 0 {
		if err := s.store.SetSessionGroup(ctx, gc.SessionID, req.GroupID); err != nil {
			s.logger.ErrorContext(ctx, "Failed to switch session group",
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to switch Loop")

			return
		}
	}

	if err := s.store.SetLastGroup(ctx, user.ID, req.GroupID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to persist last group",
			slog.String("error", err.Error()))
	}

	writeJSON(w, http.StatusOK, map[string]any{"group_id": req.GroupID})
}

// handleInstancePage renders the operator console.
// GET /instance
func (s *Server) handleInstancePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	instance, err := s.store.GetInstanceSettings(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load instance settings",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	overviews, err := s.store.ListGroupOverviews(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list groups",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	rows := make([]GroupRow, 0, len(overviews))

	for _, ov := range overviews {
		row := GroupRow{GroupOverview: ov}

		if ov.IsActive {
			if issue, err := s.store.GetActiveIssue(ctx, ov.ID); err == nil &&
				issue.Status == "collecting" {
				row.Collecting = true
				row.Deadline = issue.Deadline
			}
		}

		rows = append(rows, row)
	}

	s.renderPage(w, "instance.html", InstancePageData{
		PageData: s.newPageData(r),
		Instance: instance,
		Groups:   rows,
	})
}

// handleListGroups returns every Loop with member counts.
// GET /api/instance/groups
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	overviews, err := s.store.ListGroupOverviews(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list groups",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list groups")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": overviews})
}

// handleCreateGroup weaves a new Loop and invites its keeper (admin). The
// keeper gets an admin membership immediately and runs the Loop's setup
// wizard on their first visit.
// POST /api/instance/groups
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	operator := UserFromContext(ctx)

	var req struct {
		Name       string `json:"name"`
		AdminEmail string `json:"admin_email"`
		AdminName  string `json:"admin_name"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.AdminEmail = strings.TrimSpace(strings.ToLower(req.AdminEmail))
	req.AdminName = strings.TrimSpace(req.AdminName)

	fields := make(map[string]string, 2)

	if req.Name == "" {
		fields["name"] = "Loop name is required"
	}

	if req.AdminEmail == "" || !strings.Contains(req.AdminEmail, "@") {
		fields["admin_email"] = "A valid keeper email is required"
	}

	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	// Resolve the keeper's account FIRST: the Loop and its keeper commit in
	// one transaction below, so no failure can leave an admin-less Loop
	// behind (and a retry can't mint duplicates). A globally deactivated
	// account is restored — naming someone keeper is explicit intent that
	// they can log in.
	keeper, err := s.store.GetUserByEmail(ctx, req.AdminEmail)

	switch {
	case err == nil:
		if !keeper.IsActive {
			if err := s.store.ReactivateUser(ctx, keeper.ID); err != nil {
				s.logger.ErrorContext(ctx, "Failed to reactivate keeper",
					slog.String("error", err.Error()))
				writeError(w, http.StatusInternalServerError, "server_error", "Failed to restore keeper account")

				return
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		name := req.AdminName
		if name == "" {
			name = strings.Split(req.AdminEmail, "@")[0]
		}

		keeperID, err := s.store.CreateUser(ctx, name, req.AdminEmail)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to create keeper user",
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to create keeper")

			return
		}

		if err := s.store.EnsureNotificationPreferences(ctx, keeperID); err != nil {
			s.logger.ErrorContext(ctx, "Failed to create keeper preferences",
				slog.String("error", err.Error()))
		}

		keeper, err = s.store.GetUserByID(ctx, keeperID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to reload keeper",
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to create keeper")

			return
		}
	default:
		s.logger.ErrorContext(ctx, "Failed to look up keeper",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to look up keeper")

		return
	}

	groupID, err := s.store.CreateGroup(ctx, req.Name, &keeper.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create group",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create Loop")

		return
	}

	s.sendInviteEmail(ctx, groupID, keeper.ID, keeper.Email,
		req.Name, operator.Name, nil, nil)

	s.logger.InfoContext(ctx, "Loop created",
		slog.Int64("group_id", groupID),
		slog.String("name", req.Name),
		slog.String("keeper", keeper.Email))

	writeJSON(w, http.StatusCreated, map[string]any{"group_id": groupID})
}

// handleUpdateGroup archives or restores a Loop.
// PATCH /api/instance/groups/{id}
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	groupID, ok := s.parseIDParam(w, r, "id", "group ID")
	if !ok {
		return
	}

	var req struct {
		IsActive *bool `json:"is_active"`
	}

	if err := readJSON(r, &req); err != nil || req.IsActive == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if _, err := s.store.GetGroup(ctx, groupID); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "No such Loop")
		return
	}

	if err := s.store.SetGroupActive(ctx, groupID, *req.IsActive); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update group",
			slog.Int64("group_id", groupID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update Loop")

		return
	}

	// An archived Loop stops living: cancel its pending reminders,
	// auto-closes, and next-round events so it doesn't keep publishing and
	// emailing members who can no longer enter it. Restoring the Loop
	// doesn't resurrect them — the admin starts the next round by hand (or
	// the daily auto-create reconcile picks it back up).
	if !*req.IsActive {
		cancelled, err := s.store.DeletePendingEventsForGroup(ctx, groupID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to cancel events for archived Loop",
				slog.Int64("group_id", groupID),
				slog.String("error", err.Error()))
		} else if cancelled > 0 {
			s.logger.InfoContext(ctx, "Cancelled pending events for archived Loop",
				slog.Int64("group_id", groupID),
				slog.Int64("events", cancelled))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":  groupID,
		"is_active": *req.IsActive,
	})
}

// handleUpdateInstanceSettings applies a partial instance settings update.
// PATCH /api/instance/settings
func (s *Server) handleUpdateInstanceSettings(
	w http.ResponseWriter, r *http.Request,
) {
	ctx := r.Context()

	var req struct {
		InstanceName        *string `json:"instance_name"`
		AllowPublicMementos *bool   `json:"allow_public_mementos"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	current, err := s.store.GetInstanceSettings(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load instance settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	if req.InstanceName != nil {
		trimmed := strings.TrimSpace(*req.InstanceName)
		if trimmed == "" {
			writeValidationError(w, map[string]string{
				"instance_name": "Instance name cannot be empty",
			})

			return
		}

		current.InstanceName = trimmed
	}

	if req.AllowPublicMementos != nil {
		current.AllowPublicMementos = *req.AllowPublicMementos
	}

	if err := s.store.UpdateInstanceSettings(ctx, current); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update instance settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update settings")

		return
	}

	writeJSON(w, http.StatusOK, current)
}
