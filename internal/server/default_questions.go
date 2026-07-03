package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// updateDefaultQuestionRequest is the expected JSON body for
// PATCH /api/default-questions and PATCH /api/default-questions/{id}.
type updateDefaultQuestionRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleListDefaultQuestions returns every default question with its global
// enabled switch.
// GET /api/default-questions
func (s *Server) handleListDefaultQuestions(w http.ResponseWriter, r *http.Request) {
	questions, err := s.store.ListDefaultQuestions(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list default questions",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to list default questions")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"default_questions": questions})
}

// handleUpdateDefaultQuestion enables or disables one default question for
// all future issues. Issues that already carry the question keep their copy.
// PATCH /api/default-questions/{id}
func (s *Server) handleUpdateDefaultQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Invalid question ID")
		return
	}

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

	if err := s.store.SetDefaultQuestionEnabled(r.Context(), questionID, *req.Enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Default question not found")
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to update default question",
			slog.Int64("default_question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to update default question")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      questionID,
		"enabled": *req.Enabled,
	})
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

	if err := s.store.SetAllDefaultQuestionsEnabled(r.Context(), *req.Enabled); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update default questions",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to update default questions")

		return
	}

	questions, err := s.store.ListDefaultQuestions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": *req.Enabled})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"default_questions": questions})
}
