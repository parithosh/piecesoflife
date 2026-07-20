package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// issueColumns is the canonical issues SELECT list; its order must match the
// Scan order in scanIssue.
const issueColumns = `id, group_id, title, month, year, status, opens_at, deadline,
	published_at, created_at`

// Issue represents a newsletter issue.
type Issue struct {
	ID          int64      `json:"id"`
	GroupID     int64      `json:"group_id"`
	Title       *string    `json:"title"`
	Month       int        `json:"month"`
	Year        int        `json:"year"`
	Status      string     `json:"status"`
	OpensAt     time.Time  `json:"opens_at"`
	Deadline    time.Time  `json:"deadline"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

// scanIssue reads one issues row selected with issueColumns. Works for both
// sql.Row and sql.Rows; the scan error is returned unwrapped so callers can
// check sql.ErrNoRows.
func scanIssue(row interface{ Scan(dest ...any) error }) (*Issue, error) {
	var iss Issue

	err := row.Scan(&iss.ID, &iss.GroupID, &iss.Title, &iss.Month, &iss.Year, &iss.Status,
		&iss.OpensAt, &iss.Deadline, &iss.PublishedAt, &iss.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &iss, nil
}

// CreateIssue inserts a new issue and returns its ID.
func (s *Store) CreateIssue(
	ctx context.Context, groupID int64,
	title *string, month, year int,
	opensAt, deadline time.Time,
) (int64, error) {
	// count_admin_in starts on: admins count toward the progress
	// denominator unless they opt out via the dashboard's "Count me in
	// this round" toggle. Set explicitly because the column's schema
	// default is still 0 (SQLite can't change a default in place).
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO issues
		 (group_id, title, month, year, opens_at, deadline, count_admin_in)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		groupID, title, month, year, opensAt, deadline,
	)
	if err != nil {
		return 0, fmt.Errorf("creating issue: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new issue id: %w", err)
	}

	return id, nil
}

// GetIssueByID returns an issue by its database ID.
func (s *Store) GetIssueByID(ctx context.Context, id int64) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+` FROM issues WHERE id = ?`, id,
	))
	if err != nil {
		return nil, fmt.Errorf("getting issue %d: %w", id, err)
	}

	return iss, nil
}

// GetIssueByResponseID returns the issue that owns a response.
func (s *Store) GetIssueByResponseID(
	ctx context.Context, responseID int64,
) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT i.id, i.group_id, i.title, i.month, i.year, i.status, i.opens_at,
		        i.deadline, i.published_at, i.created_at
		 FROM issues i
		 JOIN questions q ON q.issue_id = i.id
		 JOIN responses r ON r.question_id = q.id
		 WHERE r.id = ?`, responseID,
	))
	if err != nil {
		return nil, fmt.Errorf("getting issue for response %d: %w", responseID, err)
	}

	return iss, nil
}

// GetIssueByMonthYear returns an issue by its (month, year) tuple. Used by
// onboarding retry to reuse an already-created setup issue instead of
// creating a duplicate.
func (s *Store) GetIssueByMonthYear(
	ctx context.Context, groupID int64, month, year int,
) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues WHERE group_id = ? AND month = ? AND year = ?
		 ORDER BY created_at DESC LIMIT 1`, groupID, month, year,
	))
	if err != nil {
		return nil, fmt.Errorf("getting issue for %d-%02d: %w", year, month, err)
	}

	return iss, nil
}

// GetActiveIssue returns the current draft or collecting issue.
func (s *Store) GetActiveIssue(ctx context.Context, groupID int64) (*Issue, error) {
	// A draft only counts as active once its open time has passed —
	// pre-created upcoming issues (collecting question suggestions until
	// they open) must not read as the current round.
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ?
		   AND (status = 'collecting'
		    OR (status = 'draft' AND opens_at <= ?))
		 LIMIT 1`,
		groupID, time.Now().UTC(),
	))
	if err != nil {
		return nil, fmt.Errorf("getting active issue: %w", err)
	}

	return iss, nil
}

// HasActiveIssue checks if there is a non-published issue.
func (s *Store) HasActiveIssue(ctx context.Context, groupID int64) (bool, error) {
	var exists bool

	err := s.read.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM issues
			WHERE group_id = ?
			  AND (status = 'collecting'
			   OR (status = 'draft' AND opens_at <= ?))
		)`,
		groupID, time.Now().UTC(),
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking for active issue: %w", err)
	}

	return exists, nil
}

// GetUpcomingDraftIssue returns the pre-created next round — a draft whose
// open time is still in the future — or (nil, nil) when none exists. This is
// the issue members may suggest questions to while reading the current one.
func (s *Store) GetUpcomingDraftIssue(ctx context.Context, groupID int64) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ? AND status = 'draft' AND opens_at > ?
		 ORDER BY opens_at ASC
		 LIMIT 1`,
		groupID, time.Now().UTC(),
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting upcoming draft issue: %w", err)
	}

	return iss, nil
}

// GetLatestPublishedIssue returns the group's most recently published issue,
// or (nil, nil) when nothing has been published yet. The Ramble diary window
// ("your notes since the last issue") starts at its publish date.
func (s *Store) GetLatestPublishedIssue(
	ctx context.Context, groupID int64,
) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ? AND status = 'published' AND published_at IS NOT NULL
		 ORDER BY published_at DESC
		 LIMIT 1`, groupID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting latest published issue: %w", err)
	}

	return iss, nil
}

// GetNextDraftIssue returns the earliest draft regardless of open time —
// the scheduler uses it at fire time, when the draft's opens_at has just
// passed. (nil, nil) when no draft exists.
func (s *Store) GetNextDraftIssue(ctx context.Context, groupID int64) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ? AND status = 'draft'
		 ORDER BY opens_at ASC
		 LIMIT 1`, groupID,
	))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting next draft issue: %w", err)
	}

	return iss, nil
}

// HasCollectingIssue reports whether a round is currently open for answers.
func (s *Store) HasCollectingIssue(ctx context.Context, groupID int64) (bool, error) {
	var exists bool

	err := s.read.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM issues WHERE group_id = ? AND status = 'collecting'
		)`, groupID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking for collecting issue: %w", err)
	}

	return exists, nil
}

// UpdateIssueSchedule moves a draft's open time and deadline, relabeling it
// with the (month, year) its new open date falls in — used when publish-time
// queueing, reconciliation, or a late-firing open event re-anchors a stalled
// draft. Only drafts qualify: a concurrent scheduler tick may have opened
// the round already, and rewriting a collecting round's schedule would move
// its deadline out from under members mid-collection. sql.ErrNoRows is
// returned when the row is gone or no longer a draft.
func (s *Store) UpdateIssueSchedule(
	ctx context.Context, id int64, month, year int, opensAt, deadline time.Time,
) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE issues SET month = ?, year = ?, opens_at = ?, deadline = ?
		 WHERE id = ? AND status = 'draft'`,
		month, year, opensAt, deadline, id,
	)
	if err != nil {
		return fmt.Errorf("updating issue %d schedule: %w", id, err)
	}

	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("checking issue %d schedule update: %w", id, err)
	} else if n != 1 {
		return fmt.Errorf("updating issue %d schedule: %w", id, sql.ErrNoRows)
	}

	return nil
}

// UpdateDraftIssue changes a draft's title and deadline only while it is
// still a draft. The scheduler may open the round between an HTTP handler's
// read and write; the status predicate prevents that race from moving the
// deadline without also moving the collecting round's lifecycle events.
func (s *Store) UpdateDraftIssue(
	ctx context.Context, id int64, title *string, deadline time.Time,
) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE issues SET title = COALESCE(?, title), deadline = ?
		 WHERE id = ? AND status = 'draft'`,
		title, deadline, id,
	)
	if err != nil {
		return fmt.Errorf("updating draft issue %d: %w", id, err)
	}

	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("checking draft issue %d update: %w", id, err)
	} else if n != 1 {
		return fmt.Errorf("updating draft issue %d: %w", id, sql.ErrNoRows)
	}

	return nil
}

// OpenDraftEarly atomically consumes every pending next-round event for the
// group, turns one draft into the collecting round at its new schedule, and
// installs its reminder/auto-close lifecycle. If the request is canceled or
// any statement fails, the original draft and queued opening remain intact.
func (s *Store) OpenDraftEarly(
	ctx context.Context, groupID, issueID int64,
	updateTitle bool, title *string, month, year int,
	opensAt, deadline time.Time, events []SchedulerEventSpec,
) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning early draft open: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	result, err := tx.ExecContext(ctx,
		`UPDATE issues SET
		   title = CASE WHEN ? THEN ? ELSE title END,
		   month = ?, year = ?, opens_at = ?, deadline = ?, status = 'collecting'
		 WHERE id = ? AND group_id = ? AND status = 'draft'`,
		updateTitle, title, month, year, opensAt, deadline, issueID, groupID,
	)
	if err != nil {
		return fmt.Errorf("opening draft %d early: %w", issueID, err)
	}

	if n, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("checking early open of draft %d: %w", issueID, rowsErr)
	} else if n != 1 {
		return fmt.Errorf("opening draft %d early: %w", issueID, sql.ErrNoRows)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scheduler_events
		 WHERE fired_at IS NULL AND event_type = 'create_next_issue'
		   AND issue_id IN (SELECT id FROM issues WHERE group_id = ?)`,
		groupID,
	); err != nil {
		return fmt.Errorf("consuming queued open for draft %d: %w", issueID, err)
	}

	for _, ev := range events {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO scheduler_events (issue_id, event_type, scheduled_at)
			 VALUES (?, ?, ?)`, issueID, ev.EventType, ev.ScheduledAt,
		); err != nil {
			return fmt.Errorf("queueing %s for early-opened draft %d: %w",
				ev.EventType, issueID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing early open of draft %d: %w", issueID, err)
	}

	return nil
}

// ListIssues returns issues, optionally filtered by status.
func (s *Store) ListIssues(
	ctx context.Context, groupID int64, status *string,
) ([]Issue, error) {
	var rows *sql.Rows
	var err error

	// Edition order (year, month), not creation order: a catch-up round
	// created after a later month's must not lead the archive or read as
	// the "latest edition" on /loops.
	if status != nil {
		rows, err = s.read.QueryContext(ctx,
			`SELECT `+issueColumns+`
			 FROM issues WHERE group_id = ? AND status = ?
			 ORDER BY year DESC, month DESC, id DESC`,
			groupID, *status,
		)
	} else {
		rows, err = s.read.QueryContext(ctx,
			`SELECT `+issueColumns+`
			 FROM issues WHERE group_id = ?
			 ORDER BY year DESC, month DESC, id DESC`,
			groupID,
		)
	}

	if err != nil {
		return nil, fmt.Errorf("listing issues: %w", err)
	}

	defer rows.Close()

	var issues []Issue

	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning issue: %w", err)
		}

		issues = append(issues, *iss)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating issues: %w", err)
	}

	if issues == nil {
		issues = make([]Issue, 0)
	}

	return issues, nil
}

// UpdateIssue updates an issue's title and/or deadline.
func (s *Store) UpdateIssue(
	ctx context.Context, id int64, title *string, deadline *time.Time,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE issues SET title = COALESCE(?, title),
		 deadline = COALESCE(?, deadline) WHERE id = ?`,
		title, deadline, id,
	)
	if err != nil {
		return fmt.Errorf("updating issue %d: %w", id, err)
	}

	return nil
}

// SetIssueStatus changes an issue's status.
func (s *Store) SetIssueStatus(
	ctx context.Context, id int64, status string,
) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE issues SET status = ? WHERE id = ?", status, id,
	)
	if err != nil {
		return fmt.Errorf("setting issue %d status to %s: %w", id, status, err)
	}

	return nil
}

// PublishIssue sets an issue to published with the current timestamp.
func (s *Store) PublishIssue(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE issues SET status = 'published',
		 published_at = CURRENT_TIMESTAMP WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("publishing issue %d: %w", id, err)
	}

	return nil
}
