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

// updateDefaultQuestionRequest is the expected JSON body for
// PATCH /api/default-questions and PATCH /api/default-questions/{id}.
// At least one field must be present; text is only honoured on the
// per-question route.
type updateDefaultQuestionRequest struct {
	Enabled *bool   `json:"enabled"`
	Text    *string `json:"text"`
}

// handleListDefaultQuestions returns every default question with its global
// enabled switch.
// GET /api/default-questions
func (s *Server) handleListDefaultQuestions(w http.ResponseWriter, r *http.Request) {
	questions, err := s.store.ListDefaultQuestions(r.Context(), currentGroupID(r.Context()))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list default questions",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to list default questions")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"default_questions": questions})
}

// handleUpdateDefaultQuestion rewords and/or enables/disables one default
// question for all future issues. Issues that already carry the question
// keep their copy.
// PATCH /api/default-questions/{id}
func (s *Server) handleUpdateDefaultQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	var req updateDefaultQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if req.Enabled == nil && req.Text == nil {
		writeValidationError(w, map[string]string{
			"enabled": "enabled or text is required",
		})

		return
	}

	var text *string

	if req.Text != nil {
		trimmed := strings.TrimSpace(*req.Text)
		if trimmed == "" {
			writeValidationError(w, map[string]string{
				"text": "Question text is required",
			})

			return
		}

		text = &trimmed
	}

	if err := s.store.UpdateDefaultQuestion(r.Context(), currentGroupID(r.Context()), questionID, text, req.Enabled); err != nil {
		if errors.Is(err, store.ErrDuplicateText) {
			writeValidationError(w, map[string]string{
				"text": "That question is already a default",
			})

			return
		}

		s.writeDefaultQuestionError(w, r, questionID, err, "update")

		return
	}

	question, err := s.getDefaultQuestion(r.Context(), questionID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": questionID})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"question": question})
}

// createDefaultQuestionRequest is the expected JSON body for
// POST /api/default-questions.
type createDefaultQuestionRequest struct {
	Text string `json:"text"`
}

// handleCreateDefaultQuestion adds a custom default question that will be
// stitched into every future issue.
// POST /api/default-questions
func (s *Server) handleCreateDefaultQuestion(w http.ResponseWriter, r *http.Request) {
	var req createDefaultQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeValidationError(w, map[string]string{
			"text": "Question text is required",
		})

		return
	}

	id, err := s.store.CreateDefaultQuestion(r.Context(), currentGroupID(r.Context()), text)
	if err != nil {
		// default_questions.text is UNIQUE — surface duplicates as a
		// validation problem, not a server error.
		if errors.Is(err, store.ErrDuplicateText) {
			writeValidationError(w, map[string]string{
				"text": "That question is already a default",
			})

			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to create default question",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to create default question")

		return
	}

	question, err := s.getDefaultQuestion(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"question": question})
}

// handleDeleteDefaultQuestion removes a default question from all future
// issues. Copies already landed on issues survive as ordinary questions.
// DELETE /api/default-questions/{id}
func (s *Server) handleDeleteDefaultQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	if err := s.store.DeleteDefaultQuestion(r.Context(), currentGroupID(r.Context()), questionID); err != nil {
		s.writeDefaultQuestionError(w, r, questionID, err, "delete")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// reorderDefaultQuestionsRequest is the expected JSON body for
// POST /api/default-questions/reorder.
type reorderDefaultQuestionsRequest struct {
	IDs []int64 `json:"ids"`
}

// handleReorderDefaultQuestions saves a new order for the default
// questions. The list must cover the current set exactly.
// POST /api/default-questions/reorder
func (s *Server) handleReorderDefaultQuestions(w http.ResponseWriter, r *http.Request) {
	var req reorderDefaultQuestionsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if len(req.IDs) == 0 {
		writeValidationError(w, map[string]string{
			"ids": "ids is required",
		})

		return
	}

	if err := s.store.ReorderDefaultQuestions(r.Context(), currentGroupID(r.Context()), req.IDs); err != nil {
		if errors.Is(err, store.ErrOrderMismatch) {
			writeError(w, http.StatusConflict, "stale_order",
				"The question list changed — reload and try again")
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to reorder default questions",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to reorder default questions")

		return
	}

	questions, err := s.store.ListDefaultQuestions(r.Context(), currentGroupID(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"default_questions": questions})
}

// getDefaultQuestion fetches one default question by scanning the full
// list — the table holds a handful of rows, so a dedicated query isn't
// worth the surface.
func (s *Server) getDefaultQuestion(
	ctx context.Context, id int64,
) (*store.DefaultQuestion, error) {
	questions, err := s.store.ListDefaultQuestions(ctx, currentGroupID(ctx))
	if err != nil {
		return nil, err
	}

	for i := range questions {
		if questions[i].ID == id {
			return &questions[i], nil
		}
	}

	return nil, sql.ErrNoRows
}

// writeDefaultQuestionError maps a store error on a single default question
// to the right HTTP response.
func (s *Server) writeDefaultQuestionError(
	w http.ResponseWriter, r *http.Request, questionID int64, err error, verb string,
) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "Default question not found")
		return
	}

	s.logger.ErrorContext(r.Context(), "Failed to "+verb+" default question",
		slog.Int64("default_question_id", questionID),
		slog.String("error", err.Error()))
	writeError(w, http.StatusInternalServerError, "server_error",
		"Failed to "+verb+" default question")
}

// handleUpdateAllDefaultQuestions enables or disables every default question
// at once — the "enable in all / disable for all" switch.
// PATCH /api/default-questions
func (s *Server) handleUpdateAllDefaultQuestions(w http.ResponseWriter, r *http.Request) {
	var req updateDefaultQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if req.Enabled == nil {
		writeValidationError(w, map[string]string{
			"enabled": "enabled is required",
		})

		return
	}

	if err := s.store.SetAllDefaultQuestionsEnabled(r.Context(), currentGroupID(r.Context()), *req.Enabled); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update default questions",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to update default questions")

		return
	}

	questions, err := s.store.ListDefaultQuestions(r.Context(), currentGroupID(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": *req.Enabled})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"default_questions": questions})
}
