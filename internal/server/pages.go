package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// IssueArchiveData is the template data for the issue archive page.
type IssueArchiveData struct {
	PageData
	CurrentIssue *CurrentIssueCard
	Issues       []IssueArchiveCard
	// NextIssueOpens is the pre-formatted local time the next round opens
	// (from the pending create_next_issue event). Empty when an issue is
	// open now or nothing is scheduled.
	NextIssueOpens string
}

// IssueArchiveCard bundles an issue with a small set of photo paths used to
// render the collage thumbnail on the archive page.
type IssueArchiveCard struct {
	store.Issue

	Photos []string
}

// CurrentIssueCard is the member-facing summary for the open response round.
type CurrentIssueCard struct {
	Issue         store.Issue
	QuestionCount int
	AnsweredCount int
	Submitted     bool
}

// IssuePageData is the template data for a published issue page.
type IssuePageData struct {
	PageData
	Issue         store.Issue
	Questions     []store.Question
	Responses     []store.ResponseWithBlocks
	CommentCounts map[int64]int
	// Masthead tallies for the "N people · N answers · N photos" line.
	PeopleCount int
	AnswerCount int
	PhotoCount  int
	// CurrentQ is the 1-based question the page opens on (?q=N, clamped to
	// the question count, plus one extra page when the dump exists);
	// CurrentQIdx is its 0-based twin for index math.
	CurrentQ    int
	CurrentQIdx int
	// DumpGroups is the photo & video dump grouped per contributor — the
	// collage closer page. Empty means no dump page is rendered.
	DumpGroups []DumpGroup
	// DiaryGroups is the "From the notebooks" spread: one group per member
	// who wove their rambles in. DiaryCommentCounts maps diary day IDs to
	// comment counts, DumpCommentCounts dump item IDs to theirs. PageCount
	// is questions + notebooks page (when any) + dump page (when any);
	// DiaryIdx/DumpIdx are the 0-based page indices of the extra pages, -1
	// when absent.
	DiaryGroups        []store.DiaryGroup
	DiaryCommentCounts map[int64]int
	DumpCommentCounts  map[int64]int
	PageCount          int
	DiaryIdx           int
	DumpIdx            int
	// NextIssue is the pre-created upcoming round (nil when none). Members
	// can send it question suggestions until it opens at NextIssueOpens.
	NextIssue      *store.Issue
	NextIssueOpens string
}

// RespondPageData is the template data for the response editor page.
type RespondPageData struct {
	PageData
	Issue     store.Issue
	Questions []store.Question
	Responses map[int64]*store.ResponseWithBlocks
	// Progress powers the "in this round" ledger; may be nil if it could
	// not be loaded. Answered/DaysLeft drive the ledger's segmented bar and
	// closing-soon copy.
	Progress *store.SubmissionProgress
	Answered int
	DaysLeft int
	// Submitted is true once any of the user's responses for this issue is
	// past draft. Responses stay editable until the issue is published.
	Submitted bool
	// DumpItems are the member's own photo & video dump entries for this
	// issue, shown (and managed) in the dump section before the submit bar.
	DumpItems []store.DumpItem
	// Diary is the member's attached ramble snapshot for this issue (nil
	// when not attached); DiaryDays are its editable day copies.
	Diary     *store.DiarySection
	DiaryDays []store.DiaryDayWithBlocks
	// RambleWindowCount is how many journal days fall in this Loop's diary
	// window ("since the last issue") — the opt-in card's pitch. FromLabel
	// describes the window's start for display; empty when nothing has been
	// published yet. DiaryNewCount counts journal days written after the
	// section was attached/refreshed.
	RambleWindowCount int
	RambleFromLabel   string
	DiaryNewCount     int
}

// ProfilePageData is the template data for the user profile page.
type ProfilePageData struct {
	PageData
}

// MementoPageData is the template data for a shareable response page.
// Rendered at /m/{responseID}, optionally visible without login when
// settings.AllowPublicMementos is true.
type MementoPageData struct {
	PageData

	Response  store.Response
	Blocks    []store.ResponseBlock
	Author    store.User
	Question  store.Question
	Issue     store.Issue
	Canonical string

	OGTitle       string
	OGDescription string
	OGImage       string // absolute URL, may be empty
	Public        bool   // true when viewed anonymously
}

// handleIssueArchive renders the issue archive page listing all published issues.
// GET /issues
func (s *Server) handleIssueArchive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	settings, ok := s.loadSettingsOr500(w, r)
	if !ok {
		return
	}

	publishedStatus := "published"
	issues, err := s.store.ListIssues(ctx, currentGroupID(ctx), &publishedStatus)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list issues",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Attach up to 4 photo paths per issue for the collage thumbnail.
	// Per-issue failure isn't fatal — the card just renders without photos.
	cards := make([]IssueArchiveCard, 0, len(issues))
	for _, iss := range issues {
		photos, pErr := s.store.ListPhotosForIssue(ctx, iss.ID, 4)
		if pErr != nil {
			s.logger.WarnContext(ctx, "Failed to load collage photos",
				slog.Int64("issue_id", iss.ID),
				slog.String("error", pErr.Error()))
			photos = nil
		}

		cards = append(cards, IssueArchiveCard{Issue: iss, Photos: photos})
	}

	var currentCard *CurrentIssueCard
	if currentIssue, activeErr := s.store.GetActiveIssue(ctx, currentGroupID(ctx)); activeErr == nil &&
		currentIssue.Status == "collecting" {
		questions, qErr := s.store.ListQuestionsByIssue(ctx, currentIssue.ID)
		if qErr != nil {
			s.logger.ErrorContext(ctx, "Failed to list active issue questions",
				slog.Int64("issue_id", currentIssue.ID),
				slog.String("error", qErr.Error()))
			questions = make([]store.Question, 0)
		}

		userResponses, rErr := s.store.ListUserResponsesForIssue(
			ctx, user.ID, currentIssue.ID,
		)
		if rErr != nil {
			s.logger.ErrorContext(ctx, "Failed to list active issue user responses",
				slog.Int64("issue_id", currentIssue.ID),
				slog.Int64("user_id", user.ID),
				slog.String("error", rErr.Error()))
			userResponses = make([]store.Response, 0)
		}

		submitted := false
		for _, response := range userResponses {
			if !response.IsDraft {
				submitted = true
				break
			}
		}

		currentCard = &CurrentIssueCard{
			Issue:         *currentIssue,
			QuestionCount: len(questions),
			AnsweredCount: len(userResponses),
			Submitted:     submitted,
		}
	}

	var nextIssueOpens string
	if currentCard == nil {
		nextIssueOpens = s.nextIssueOpensLabel(ctx, settings)
	}

	data := IssueArchiveData{
		PageData:       s.newPageData(r),
		CurrentIssue:   currentCard,
		Issues:         cards,
		NextIssueOpens: nextIssueOpens,
	}

	s.renderPage(w, "issues_archive.html", data)
}

// publishedBlockRank orders answer blocks for the reading view: prose first,
// then links, photos, audio, and video. Unknown types sort last.
func publishedBlockRank(blockType string) int {
	switch blockType {
	case "text":
		return 0
	case "link":
		return 1
	case "photo":
		return 2
	case "audio":
		return 3
	case "video":
		return 4
	default:
		return 5
	}
}

// handleIssuePage renders a single published issue.
// GET /issues/{year}/{month}
func (s *Server) handleIssuePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	yearStr := r.PathValue("year")
	monthStr := r.PathValue("month")

	year, err := strconv.Atoi(yearStr)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	month, err := strconv.Atoi(monthStr)
	if err != nil || month < 1 || month > 12 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Find the issue matching year/month among this Loop's published issues.
	allIssues, err := s.store.ListIssues(ctx, currentGroupID(ctx), nil)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list issues",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var targetIssue *store.Issue
	for i := range allIssues {
		iss := &allIssues[i]
		if iss.Year == year && iss.Month == month && iss.Status == "published" {
			targetIssue = iss
			break
		}
	}

	if targetIssue == nil {
		http.NotFound(w, r)
		return
	}

	settings, ok := s.loadSettingsOr500(w, r)
	if !ok {
		return
	}

	questions, err := s.store.ListQuestionsByIssue(ctx, targetIssue.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list questions",
			slog.Int64("issue_id", targetIssue.ID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	rawResponses, err := s.store.ListResponsesByIssue(ctx, targetIssue.ID, true)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list responses",
			slog.Int64("issue_id", targetIssue.ID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Build ResponseWithBlocks for each submitted response.
	responses := make([]store.ResponseWithBlocks, 0, len(rawResponses))
	for _, resp := range rawResponses {
		blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list blocks",
				slog.Int64("response_id", resp.ID),
				slog.String("error", err.Error()))
			blocks = make([]store.ResponseBlock, 0)
		}

		// Reading order for each member's answer: text, then links, then
		// photos, then audio, then video (stable within each type). The
		// editor keeps the member's own arrangement; this applies to the
		// published view only.
		sort.SliceStable(blocks, func(i, j int) bool {
			return publishedBlockRank(blocks[i].Type) < publishedBlockRank(blocks[j].Type)
		})

		author, err := s.store.GetUserByID(ctx, resp.UserID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to get response author",
				slog.Int64("user_id", resp.UserID),
				slog.String("error", err.Error()))
			continue
		}

		responses = append(responses, store.ResponseWithBlocks{
			Response: resp,
			Blocks:   blocks,
			User:     *author,
		})
	}

	// Tally masthead counts: distinct authors, total answers, and photos.
	people := make(map[int64]struct{}, len(responses))
	commentCounts := make(map[int64]int, len(responses))
	photoCount := 0

	for i := range responses {
		people[responses[i].Response.UserID] = struct{}{}
		for j := range responses[i].Blocks {
			if responses[i].Blocks[j].Type == "photo" {
				photoCount++
			}
		}

		comments, cErr := s.store.ListCommentsByResponse(ctx, responses[i].Response.ID)
		if cErr != nil {
			s.logger.WarnContext(ctx, "Failed to count comments",
				slog.Int64("response_id", responses[i].Response.ID),
				slog.String("error", cErr.Error()))
			continue
		}
		commentCounts[responses[i].Response.ID] = len(comments)
	}

	dumpItems, err := s.store.ListDumpItemsByIssue(ctx, targetIssue.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list dump items",
			slog.Int64("issue_id", targetIssue.ID),
			slog.String("error", err.Error()))
		dumpItems = make([]store.DumpItemWithUser, 0)
	}

	dumpGroups := groupDumpItems(dumpItems)

	dumpCommentCounts := make(map[int64]int, len(dumpItems))

	for i := range dumpItems {
		comments, cErr := s.store.ListCommentsByDumpItem(ctx, dumpItems[i].ID)
		if cErr != nil {
			s.logger.WarnContext(ctx, "Failed to count dump comments",
				slog.Int64("dump_item_id", dumpItems[i].ID),
				slog.String("error", cErr.Error()))

			continue
		}

		dumpCommentCounts[dumpItems[i].ID] = len(comments)
	}

	// The notebooks spread: diary sections members wove in, plus per-day
	// comment counts (day comments use the same panels answers do).
	diaryGroups, err := s.store.ListDiarySectionsByIssue(ctx, targetIssue.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list diary sections",
			slog.Int64("issue_id", targetIssue.ID),
			slog.String("error", err.Error()))
		diaryGroups = make([]store.DiaryGroup, 0)
	}

	diaryCommentCounts := make(map[int64]int, 16)

	for gi := range diaryGroups {
		for di := range diaryGroups[gi].Days {
			day := &diaryGroups[gi].Days[di]

			for bi := range day.Blocks {
				if day.Blocks[bi].Type == "photo" {
					photoCount++
				}
			}

			comments, cErr := s.store.ListCommentsByDiaryDay(ctx, day.DiaryDay.ID)
			if cErr != nil {
				s.logger.WarnContext(ctx, "Failed to count diary comments",
					slog.Int64("day_id", day.DiaryDay.ID),
					slog.String("error", cErr.Error()))
				continue
			}

			diaryCommentCounts[day.DiaryDay.ID] = len(comments)
		}
	}

	// Surface the pre-created next round so readers can send it question
	// suggestions while the current issue is fresh in their minds.
	var nextIssue *store.Issue
	var nextIssueOpens string
	if draft, dErr := s.store.GetUpcomingDraftIssue(ctx, currentGroupID(ctx)); dErr != nil {
		s.logger.WarnContext(ctx, "Failed to look up upcoming draft for issue page",
			slog.String("error", dErr.Error()))
	} else if draft != nil {
		nextIssue = draft
		loc := s.settingsLocation(ctx, settings)
		nextIssueOpens = draft.OpensAt.In(loc).Format("Monday, 2 Jan")
	}

	// Honor ?q=N deep links server-side so the correct question renders
	// (and the pager links are right) before any JavaScript runs. Extra
	// pages come after the questions: the notebooks spread (when anyone
	// wove theirs in), then the dump collage as the closer.
	pageCount := len(questions)
	diaryIdx, dumpIdx := -1, -1

	if len(diaryGroups) > 0 {
		diaryIdx = pageCount
		pageCount++
	}

	if len(dumpGroups) > 0 {
		dumpIdx = pageCount
		pageCount++
	}

	currentQ := 1
	if parsed, qErr := strconv.Atoi(r.URL.Query().Get("q")); qErr == nil && parsed > 1 {
		currentQ = parsed
	}
	if pageCount > 0 && currentQ > pageCount {
		currentQ = pageCount
	}

	data := IssuePageData{
		PageData:           s.newPageData(r),
		Issue:              *targetIssue,
		Questions:          questions,
		Responses:          responses,
		CommentCounts:      commentCounts,
		PeopleCount:        len(people),
		AnswerCount:        len(responses),
		PhotoCount:         photoCount,
		CurrentQ:           currentQ,
		CurrentQIdx:        currentQ - 1,
		DumpGroups:         dumpGroups,
		DumpCommentCounts:  dumpCommentCounts,
		DiaryGroups:        diaryGroups,
		DiaryCommentCounts: diaryCommentCounts,
		PageCount:          pageCount,
		DiaryIdx:           diaryIdx,
		DumpIdx:            dumpIdx,
		NextIssue:          nextIssue,
		NextIssueOpens:     nextIssueOpens,
	}

	s.renderPage(w, "issue.html", data)
}

// handleRespondPage renders the response editor for the current user.
// GET /issues/{id}/respond
func (s *Server) handleRespondPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	idStr := r.PathValue("id")

	issueID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	// Only allow responding to issues that are actively collecting.
	if issue.Status != "collecting" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	questions, err := s.store.ListQuestionsByIssue(ctx, issueID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list questions",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Load the current user's responses for this issue.
	userResponses, err := s.store.ListUserResponsesForIssue(ctx, user.ID, issueID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load user responses",
			slog.Int64("user_id", user.ID),
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		userResponses = make([]store.Response, 0)
	}

	// Build a map from question ID -> ResponseWithBlocks, counting how many
	// prompts the user has meaningfully answered for the ledger's bar.
	responseMap := make(map[int64]*store.ResponseWithBlocks, len(userResponses))
	answered := 0
	submitted := false

	for i := range userResponses {
		resp := userResponses[i]

		if !resp.IsDraft {
			submitted = true
		}

		blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list blocks for user response",
				slog.Int64("response_id", resp.ID),
				slog.String("error", err.Error()))
			blocks = make([]store.ResponseBlock, 0)
		}

		responseMap[resp.QuestionID] = &store.ResponseWithBlocks{
			Response: resp,
			Blocks:   blocks,
			User:     *user,
		}

		if hasAnswerContent(blocks) {
			answered++
		}
	}

	// The ledger roster ("in this round") reuses submission progress. A
	// failure here is non-fatal — the editor still renders without it.
	progress, err := s.store.GetSubmissionProgress(ctx, issueID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load progress for respond ledger",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		progress = nil
	}

	daysLeft := int(time.Until(issue.Deadline).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}

	dumpItems, err := s.store.ListDumpItemsForUser(ctx, issueID, user.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list dump items for respond page",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		dumpItems = make([]store.DumpItem, 0)
	}

	data := RespondPageData{
		PageData:  s.newPageData(r),
		Issue:     *issue,
		Questions: questions,
		Responses: responseMap,
		Progress:  progress,
		Answered:  answered,
		DaysLeft:  daysLeft,
		Submitted: submitted,
		DumpItems: dumpItems,
	}

	s.loadRespondDiary(ctx, issue, user.ID, &data)

	s.renderPage(w, "respond.html", data)
}

// loadRespondDiary fills the respond page's ramble card: either the pitch
// (how many journal days the diary window holds) or the attached section's
// editable day copies plus the pull-in count. All failures are non-fatal —
// the editor renders without the card's numbers rather than not at all.
func (s *Server) loadRespondDiary(
	ctx context.Context, issue *store.Issue, userID int64, data *RespondPageData,
) {
	fromDay, throughDay := s.diaryWindow(ctx, issue.GroupID)

	if fromDay != "" {
		data.RambleFromLabel = rambleDayDisplay(fromDay)
	}

	section, err := s.store.GetDiarySection(ctx, issue.ID, userID)
	if err != nil {
		// Not attached yet: pitch the window.
		count, cErr := s.store.CountRambleDaysBetween(ctx, userID, fromDay, throughDay)
		if cErr != nil {
			s.logger.WarnContext(ctx, "Failed to count ramble window",
				slog.Int64("user_id", userID),
				slog.String("error", cErr.Error()))
			return
		}

		data.RambleWindowCount = count

		return
	}

	data.Diary = section

	days, err := s.store.ListDiaryDays(ctx, section.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list diary days for respond page",
			slog.Int64("section_id", section.ID),
			slog.String("error", err.Error()))
		days = make([]store.DiaryDayWithBlocks, 0)
	}

	data.DiaryDays = days

	newCount, err := s.store.CountPullableRambleDays(
		ctx, userID, section.ID, throughDay)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to count new ramble days",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		return
	}

	data.DiaryNewCount = newCount
}

// handleAlbumsPage renders the photo albums page.
// GET /albums
func (s *Server) handleAlbumsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "albums.html", s.newPageData(r))
}

// handleProfilePage renders the current user's profile page.
// GET /profile
func (s *Server) handleProfilePage(w http.ResponseWriter, r *http.Request) {

	data := ProfilePageData{
		PageData: s.newPageData(r),
	}

	s.renderPage(w, "profile.html", data)
}

// handleUploadServe serves uploaded files with directory traversal protection.
// GET /uploads/*
func (s *Server) handleUploadServe(w http.ResponseWriter, r *http.Request) {
	// Strip the /uploads/ prefix to get the relative path.
	relPath := strings.TrimPrefix(r.URL.Path, "/uploads/")

	// Reject anything that could resolve outside the upload directory.
	// filepath.Rel catches both ".." traversal and sibling-prefix attacks
	// like "/data/uploads2" passing a bare HasPrefix("/data/uploads").
	base := filepath.Clean(s.config.UploadPath)
	cleanPath := filepath.Clean(filepath.Join(base, relPath))

	rel, err := filepath.Rel(base, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, cleanPath)
}

// handleListUsers returns all active users as JSON.
// GET /api/users
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	users, err := s.store.ListActiveGroupMembers(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list users",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list users")
		return
	}

	writeJSON(w, http.StatusOK, ListResponse[store.GroupMember]{
		Items:   users,
		Total:   len(users),
		Page:    1,
		PerPage: len(users),
	})
}

// handleUpdateUser updates a user's name, bio, and/or avatar URL.
// PATCH /api/users/{id}
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := UserFromContext(ctx)

	userID, ok := s.parseIDParam(w, r, "id", "user ID")
	if !ok {
		return
	}

	// Members may only update their own profile; Loop admins may update any
	// member of their Loop.
	if caller.ID != userID && !s.isGroupAdminOver(ctx, userID) {
		writeError(w, http.StatusForbidden, "forbidden", "Cannot update another user's profile")
		return
	}

	var req struct {
		Name      *string `json:"name"`
		AvatarURL *string `json:"avatar_url"`
		Bio       *string `json:"bio"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	target, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get user for update",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	// Apply partial updates.
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeValidationError(w, map[string]string{"name": "Name cannot be empty"})
			return
		}
		target.Name = trimmed
	}

	if req.AvatarURL != nil {
		target.AvatarURL = req.AvatarURL
	}

	if req.Bio != nil {
		target.Bio = req.Bio
	}

	if err := s.store.UpdateUser(ctx, target.ID, target.Name, target.AvatarURL, target.Bio); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update user",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update user")
		return
	}

	s.logger.InfoContext(ctx, "User updated",
		slog.Int64("user_id", userID))

	writeJSON(w, http.StatusOK, target)
}

// handleGetPreferences returns a user's notification preferences.
// GET /api/users/{id}/preferences
func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := UserFromContext(ctx)

	userID, ok := s.parseIDParam(w, r, "id", "user ID")
	if !ok {
		return
	}

	// Members may only view their own preferences.
	if caller.ID != userID && !s.isGroupAdminOver(ctx, userID) {
		writeError(w, http.StatusForbidden, "forbidden", "Cannot view another user's preferences")
		return
	}

	prefs, err := s.store.GetNotificationPreferences(ctx, userID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get notification preferences",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get preferences")
		return
	}

	writeJSON(w, http.StatusOK, prefs)
}

// handleUpdatePreferences updates a user's notification preferences.
// PATCH /api/users/{id}/preferences
func (s *Server) handleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller := UserFromContext(ctx)

	userID, ok := s.parseIDParam(w, r, "id", "user ID")
	if !ok {
		return
	}

	// Members may only update their own preferences.
	if caller.ID != userID && !s.isGroupAdminOver(ctx, userID) {
		writeError(w, http.StatusForbidden, "forbidden", "Cannot update another user's preferences")
		return
	}

	var req struct {
		IssueOpen     *bool `json:"issue_open"`
		Reminders     *bool `json:"reminders"`
		Published     *bool `json:"published"`
		CommentNotify *bool `json:"comment_notify"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	current, err := s.store.GetNotificationPreferences(ctx, userID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get notification preferences",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load preferences")
		return
	}

	// Apply partial updates.
	if req.IssueOpen != nil {
		current.IssueOpen = *req.IssueOpen
	}

	if req.Reminders != nil {
		current.Reminders = *req.Reminders
	}

	if req.Published != nil {
		current.Published = *req.Published
	}

	if req.CommentNotify != nil {
		current.CommentNotify = *req.CommentNotify
	}

	if err := s.store.UpsertNotificationPreferences(ctx, current); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update notification preferences",
			slog.Int64("user_id", userID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update preferences")
		return
	}

	s.logger.InfoContext(ctx, "Notification preferences updated",
		slog.Int64("user_id", userID))

	writeJSON(w, http.StatusOK, current)
}

// mementoAccess evaluates the memento access gate shared by the page and
// file handlers: whether the viewer is an active member of the owning Loop
// (instance admins pass), and whether public viewing is allowed — the
// Loop's switch ANDed with the instance policy.
func (s *Server) mementoAccess(
	ctx context.Context, viewer *store.User, groupID int64,
	settings *store.Settings,
) (isMember, publicAllowed bool) {
	if viewer != nil {
		if viewer.IsInstanceAdmin {
			isMember = true
		} else if m, err := s.store.GetMembership(ctx, groupID, viewer.ID); err == nil &&
			m.IsActive {
			isMember = true
		}
	}

	publicAllowed = settings.AllowPublicMementos

	if inst, err := s.store.GetInstanceSettings(ctx); err == nil {
		publicAllowed = publicAllowed && inst.AllowPublicMementos
	}

	return isMember, publicAllowed
}

// handleMemento renders a single response as a shareable page with Open
// Graph tags so link previews on social/chat apps look nice. The page is
// public iff settings.AllowPublicMementos is true AND the parent issue is
// published — drafts are never public even with the setting on.
// GET /m/{id}
func (s *Server) handleMemento(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	responseID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	resp, err := s.store.GetResponseByID(ctx, responseID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if resp.IsDraft {
		http.NotFound(w, r)
		return
	}

	question, err := s.store.GetQuestionByID(ctx, resp.QuestionID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	issue, err := s.store.GetIssueByID(ctx, question.IssueID)
	if err != nil || issue.Status != "published" {
		http.NotFound(w, r)
		return
	}

	// This route is public (no auth middleware), so settings come from the
	// owning issue's Loop, not a session's current one.
	settings, err := s.store.GetSettings(ctx, issue.GroupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings for memento",
			slog.Int64("group_id", issue.GroupID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	// Access control: a signed-in active member of the owning Loop (or an
	// instance admin) always may view. Anyone else counts as a public
	// visitor, which requires the Loop's public-memento switch AND the
	// instance policy.
	viewer := s.lookupSessionUser(r)

	isMember, publicAllowed := s.mementoAccess(ctx, viewer, issue.GroupID, settings)

	if !isMember && !publicAllowed {
		// Anonymous visitors might just need to log in; a signed-in
		// non-member gets a plain 404 — a login form can't help them.
		if viewer == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		http.NotFound(w, r)

		return
	}

	blocks, err := s.store.ListBlocksByResponse(ctx, responseID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load memento blocks",
			slog.Int64("response_id", responseID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	author, err := s.store.GetUserByID(ctx, resp.UserID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load memento author",
			slog.Int64("user_id", resp.UserID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	ogTitle := fmt.Sprintf("%s on: %s", author.Name, question.Text)
	ogDesc := summarizeBlocks(blocks, 220)

	var ogImage string
	for _, b := range blocks {
		if b.Type == "photo" && b.FilePath != nil {
			// Use the memento-scoped file route so anonymous viewers can
			// fetch the OG image even though /uploads/ is auth-gated.
			ogImage = s.config.BaseURL + s.mementoFileURL(responseID, *b.FilePath)
			break
		}
	}

	data := MementoPageData{
		PageData: PageData{
			User:     viewer,
			Settings: settings,
		},
		Response:      *resp,
		Blocks:        blocks,
		Author:        *author,
		Question:      *question,
		Issue:         *issue,
		Canonical:     fmt.Sprintf("%s/m/%d", s.config.BaseURL, responseID),
		OGTitle:       ogTitle,
		OGDescription: ogDesc,
		OGImage:       ogImage,
		Public:        !isMember,
	}

	s.renderPage(w, "memento.html", data)
}

// handleMementoFile serves an upload referenced by a public-or-owned memento.
// It mirrors handleMemento's access checks (issue published, viewer authed
// or AllowPublicMementos on) and additionally requires the requested file
// path to be referenced by some block of the memento — so this route can't
// be used as an open proxy for arbitrary uploads.
// GET /m/{id}/file/{path...}
func (s *Server) handleMementoFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	responseID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	relPath := r.PathValue("path")
	if relPath == "" {
		http.NotFound(w, r)
		return
	}

	resp, err := s.store.GetResponseByID(ctx, responseID)
	if err != nil || resp.IsDraft {
		http.NotFound(w, r)
		return
	}

	question, err := s.store.GetQuestionByID(ctx, resp.QuestionID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	issue, err := s.store.GetIssueByID(ctx, question.IssueID)
	if err != nil || issue.Status != "published" {
		http.NotFound(w, r)
		return
	}

	settings, err := s.store.GetSettings(ctx, issue.GroupID)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Same gate as handleMemento; files always deny with a plain 404.
	viewer := s.lookupSessionUser(r)

	isMember, publicAllowed := s.mementoAccess(ctx, viewer, issue.GroupID, settings)

	if !isMember && !publicAllowed {
		http.NotFound(w, r)
		return
	}

	blocks, err := s.store.ListBlocksByResponse(ctx, responseID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load memento blocks for file",
			slog.Int64("response_id", responseID),
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	base := filepath.Clean(s.config.UploadPath)
	cleanPath := filepath.Clean(filepath.Join(base, relPath))

	rel, err := filepath.Rel(base, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Require that the resolved path matches some block on this memento.
	// This prevents the route from acting as a generic public proxy.
	matched := false
	for _, b := range blocks {
		if b.FilePath == nil {
			continue
		}
		if filepath.Clean(*b.FilePath) == cleanPath {
			matched = true
			break
		}
	}

	if !matched {
		http.NotFound(w, r)
		return
	}

	http.ServeFile(w, r, cleanPath)
}

// lookupSessionUser attempts to resolve an active session cookie into a
// *store.User, returning nil (not an error) for any failure. Used by
// handlers that need optional-auth behaviour (the memento route) outside
// the usual authMiddleware chain.
func (s *Server) lookupSessionUser(r *http.Request) *store.User {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}

	sess, err := s.store.GetSessionByHash(r.Context(), hashSessionToken(cookie.Value))
	if err != nil || sess.ExpiresAt.Before(time.Now()) {
		return nil
	}

	user, err := s.store.GetUserByID(r.Context(), sess.UserID)
	if err != nil || !user.IsActive {
		return nil
	}

	return user
}

// summarizeBlocks returns the first text block's content truncated to max
// characters, preserving word boundaries. Used for OG descriptions.
func summarizeBlocks(blocks []store.ResponseBlock, max int) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Content != nil {
			return truncateWords(*b.Content, max)
		}
	}
	return ""
}

func truncateWords(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}

	cut := s[:max]
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}

	return strings.TrimSpace(cut) + "…"
}

// mediaEntry is one item on the Media page: any photo, video, or audio ever
// shared into a published issue — answer blocks and photo-dump items alike.
type mediaEntry struct {
	Kind         string    `json:"kind"` // photo | audio | video
	URL          string    `json:"url"`
	Caption      *string   `json:"caption"`
	CreatedAt    time.Time `json:"created_at"`
	UserName     string    `json:"user_name"`
	FromDump     bool      `json:"from_dump"`
	FromNotebook bool      `json:"from_notebook"`
	IssueID      int64     `json:"issue_id"`
	Title        *string   `json:"title"`
	Month        int       `json:"month"`
	Year         int       `json:"year"`
}

// handleListAlbums returns every media item (photos, videos, audio — from
// answers and from the photo & video dump) across published issues as JSON.
// Each entry's URL is the browser-accessible path (the raw file_path is a
// disk path that would 404 if given to an <img> src).
// GET /api/albums
func (s *Server) handleListAlbums(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	publishedStatus := "published"
	issues, err := s.store.ListIssues(ctx, currentGroupID(ctx), &publishedStatus)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list published issues for albums",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list issues")
		return
	}

	media := make([]mediaEntry, 0)

	for _, issue := range issues {
		responses, err := s.store.ListResponsesByIssue(ctx, issue.ID, true)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list responses for album",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
			continue
		}

		for _, resp := range responses {
			blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to list blocks for album response",
					slog.Int64("response_id", resp.ID),
					slog.String("error", err.Error()))
				continue
			}

			author, err := s.store.GetUserByID(ctx, resp.UserID)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to get response author for album",
					slog.Int64("user_id", resp.UserID),
					slog.String("error", err.Error()))
				continue
			}

			for _, block := range blocks {
				if block.Type != "photo" && block.Type != "audio" && block.Type != "video" {
					continue
				}
				if block.FilePath == nil || *block.FilePath == "" {
					continue
				}

				media = append(media, mediaEntry{
					Kind:      block.Type,
					URL:       s.uploadURL(*block.FilePath),
					Caption:   block.Caption,
					CreatedAt: block.CreatedAt,
					UserName:  author.Name,
					IssueID:   issue.ID,
					Title:     issue.Title,
					Month:     issue.Month,
					Year:      issue.Year,
				})
			}
		}

		dumpItems, err := s.store.ListDumpItemsByIssue(ctx, issue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list dump items for album",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
			continue
		}

		for _, item := range dumpItems {
			media = append(media, mediaEntry{
				Kind:      item.Kind,
				URL:       s.uploadURL(item.FilePath),
				Caption:   item.Caption,
				CreatedAt: item.CreatedAt,
				UserName:  item.UserName,
				FromDump:  true,
				IssueID:   issue.ID,
				Title:     issue.Title,
				Month:     issue.Month,
				Year:      issue.Year,
			})
		}

		// Published notebook media. Only what members wove into the issue —
		// unattached journal media never appears here.
		diaryGroups, err := s.store.ListDiarySectionsByIssue(ctx, issue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list diary sections for album",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
			continue
		}

		for _, g := range diaryGroups {
			for _, d := range g.Days {
				for _, b := range d.Blocks {
					if b.Type == "text" || b.FilePath == nil || *b.FilePath == "" {
						continue
					}

					media = append(media, mediaEntry{
						Kind:         b.Type,
						URL:          s.uploadURL(*b.FilePath),
						Caption:      b.Caption,
						CreatedAt:    b.CreatedAt,
						UserName:     g.UserName,
						FromNotebook: true,
						IssueID:      issue.ID,
						Title:        issue.Title,
						Month:        issue.Month,
						Year:         issue.Year,
					})
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, ListResponse[mediaEntry]{
		Items:   media,
		Total:   len(media),
		Page:    1,
		PerPage: len(media),
	})
}

// uploadURL converts an on-disk upload path into the browser-accessible URL
// served by handleUploadServe. Matches the template helper of the same name.
//
// The prefix match must be followed by a path separator so a sibling
// directory (e.g. /data/uploads2) doesn't get rewritten as /uploads2 — a
// naive HasPrefix(p, "/data/uploads") would accept it.
func (s *Server) uploadURL(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "/uploads/") || strings.HasPrefix(p, "http") {
		return p
	}

	prefix := filepath.Clean(s.config.UploadPath)
	sep := string(filepath.Separator)

	if p == prefix {
		return "/uploads"
	}

	if strings.HasPrefix(p, prefix+sep) {
		rel := strings.TrimPrefix(p, prefix)
		return "/uploads" + strings.ReplaceAll(rel, sep, "/")
	}

	return p
}

// mementoFileURL produces a public URL for a memento's file via the
// /m/{id}/file/... route, which permits anonymous viewers when
// AllowPublicMementos is on. Falls back to uploadURL behaviour for paths
// that already look like URLs or sit outside the configured UploadPath.
func (s *Server) mementoFileURL(responseID int64, p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "http") {
		return p
	}

	prefix := filepath.Clean(s.config.UploadPath)
	sep := string(filepath.Separator)

	var rel string
	switch {
	case p == prefix:
		rel = ""
	case strings.HasPrefix(p, prefix+sep):
		rel = strings.TrimPrefix(p, prefix+sep)
	default:
		return s.uploadURL(p)
	}

	return fmt.Sprintf("/m/%d/file/%s", responseID, strings.ReplaceAll(rel, sep, "/"))
}

// hasAnswerContent reports whether a response's blocks hold anything a reader
// would see — non-empty text, media, or a link. Used to count meaningfully
// answered prompts for the respond ledger's progress bar.
func hasAnswerContent(blocks []store.ResponseBlock) bool {
	for i := range blocks {
		b := blocks[i]
		switch b.Type {
		case "text":
			if b.Content != nil && strings.TrimSpace(*b.Content) != "" {
				return true
			}
		case "photo", "audio", "video":
			if b.FilePath != nil && *b.FilePath != "" {
				return true
			}
		case "link":
			if b.LinkURL != nil && *b.LinkURL != "" {
				return true
			}
		}
	}

	return false
}
