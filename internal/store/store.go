// Package store persists tasks, conversation history, timeline events,
// token usage and approval requests in SQLite.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Task statuses.
const (
	StatusTodo             = "todo"
	StatusInProgress       = "in_progress"
	StatusAwaitingApproval = "awaiting_approval"
	StatusBlocked          = "blocked"
	StatusCompleted        = "completed"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Task struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Prompt           string    `json:"prompt"`
	Status           string    `json:"status"`
	Workdir          string    `json:"workdir"`
	Model            string    `json:"model"`
	BudgetUSD        float64   `json:"budget_usd"`
	CostUSD          float64   `json:"cost_usd"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	Iterations       int       `json:"iterations"`
	Epoch            int       `json:"epoch"`
	BlockedReason    string    `json:"blocked_reason"`
	Summary          string    `json:"summary"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

const taskCols = `id, title, prompt, status, workdir, model, budget_usd, cost_usd, input_tokens, output_tokens,
	cache_read_tokens, cache_write_tokens, iterations, epoch, blocked_reason, summary, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var created, updated int64
	err := row.Scan(&t.ID, &t.Title, &t.Prompt, &t.Status, &t.Workdir, &t.Model, &t.BudgetUSD, &t.CostUSD,
		&t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheWriteTokens, &t.Iterations, &t.Epoch,
		&t.BlockedReason, &t.Summary, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	t.UpdatedAt = time.UnixMilli(updated).UTC()
	return &t, nil
}

func now() int64 { return time.Now().UnixMilli() }

func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	ts := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (`+taskCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Prompt, StatusTodo, t.Workdir, t.Model, t.BudgetUSD, 0, 0, 0, 0, 0, 0, 0, "", "", ts, ts)
	if err != nil {
		return err
	}
	t.Status = StatusTodo
	t.CreatedAt = time.UnixMilli(ts).UTC()
	t.UpdatedAt = t.CreatedAt
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

func (s *Store) ListTasks(ctx context.Context, limit int) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetStatus changes a task's status. reason is stored for blocked tasks and
// cleared otherwise.
func (s *Store) SetStatus(ctx context.Context, id, status, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, blocked_reason = ?, updated_at = ? WHERE id = ?`,
		status, reason, now(), id)
	return err
}

func (s *Store) SetBudget(ctx context.Context, id string, usd float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET budget_usd = ?, updated_at = ? WHERE id = ?`, usd, now(), id)
	return err
}

func (s *Store) SetSummary(ctx context.Context, id, summary string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET summary = ?, updated_at = ? WHERE id = ?`, summary, now(), id)
	return err
}

func (s *Store) IncIterations(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE tasks SET iterations = iterations + 1, updated_at = ? WHERE id = ? RETURNING iterations`, now(), id).Scan(&n)
	return n, err
}

// MarkInterrupted moves tasks that were running when the process stopped to
// blocked, so they can be resumed explicitly.
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, blocked_reason = ?, updated_at = ? WHERE status = ?`,
		StatusBlocked, "interrupted: the server stopped while this task was running", now(), StatusInProgress)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- conversation history ----

// Message kinds stored in the messages table.
const (
	KindAssistant = "assistant" // raw API response JSON
	KindUser      = "user"      // JSON array of UserBlock
)

type Message struct {
	ID    int64
	Epoch int
	Kind  string
	Body  json.RawMessage
}

func (s *Store) AppendMessage(ctx context.Context, taskID string, epoch int, kind string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages (task_id, epoch, kind, body, created_at) VALUES (?,?,?,?,?)`,
		taskID, epoch, kind, string(body), now())
	return err
}

// Messages returns the conversation for the task's given epoch, oldest first.
func (s *Store) Messages(ctx context.Context, taskID string, epoch int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, epoch, kind, body FROM messages WHERE task_id = ? AND epoch = ? ORDER BY id`, taskID, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var body string
		if err := rows.Scan(&m.ID, &m.Epoch, &m.Kind, &body); err != nil {
			return nil, err
		}
		m.Body = json.RawMessage(body)
		out = append(out, m)
	}
	return out, rows.Err()
}

// StartEpoch begins a fresh conversation for the task (used after compaction).
func (s *Store) StartEpoch(ctx context.Context, taskID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`UPDATE tasks SET epoch = epoch + 1, updated_at = ? WHERE id = ? RETURNING epoch`, now(), taskID).Scan(&n)
	return n, err
}

// ---- events ----

type Event struct {
	ID     int64           `json:"id"`
	TaskID string          `json:"task_id"`
	Time   time.Time       `json:"time"`
	Type   string          `json:"type"`
	Data   json.RawMessage `json:"data"`
}

func (s *Store) AddEvent(ctx context.Context, taskID, typ string, data any) (*Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	ts := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (task_id, ts, type, data) VALUES (?,?,?,?)`,
		taskID, ts, typ, string(b))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Event{ID: id, TaskID: taskID, Time: time.UnixMilli(ts).UTC(), Type: typ, Data: b}, nil
}

// Events returns events with id > afterID, oldest first.
func (s *Store) Events(ctx context.Context, taskID string, afterID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, ts, type, data FROM events WHERE task_id = ? AND id > ? ORDER BY id`, taskID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		var data string
		if err := rows.Scan(&e.ID, &e.TaskID, &ts, &e.Type, &data); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(ts).UTC()
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- usage ----

type Usage struct {
	ID               int64     `json:"id"`
	TaskID           string    `json:"task_id"`
	Time             time.Time `json:"time"`
	Purpose          string    `json:"purpose"` // "step" or "compaction"
	Model            string    `json:"model"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
}

// RecordUsage stores one API call and adds it to the task's running totals.
func (s *Store) RecordUsage(ctx context.Context, u *Usage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage (task_id, ts, purpose, model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd) VALUES (?,?,?,?,?,?,?,?,?)`,
		u.TaskID, ts, u.Purpose, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET cost_usd = cost_usd + ?, input_tokens = input_tokens + ?,
		output_tokens = output_tokens + ?, cache_read_tokens = cache_read_tokens + ?, cache_write_tokens = cache_write_tokens + ?,
		updated_at = ? WHERE id = ?`,
		u.CostUSD, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, ts, u.TaskID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UsageForTask(ctx context.Context, taskID string) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, ts, purpose, model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd FROM usage WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		var ts int64
		if err := rows.Scan(&u.ID, &u.TaskID, &ts, &u.Purpose, &u.Model, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &u.CostUSD); err != nil {
			return nil, err
		}
		u.Time = time.UnixMilli(ts).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

type Totals struct {
	Tasks        int            `json:"tasks"`
	ByStatus     map[string]int `json:"by_status"`
	CostUSD      float64        `json:"cost_usd"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	CacheRead    int64          `json:"cache_read_tokens"`
}

func (s *Store) Totals(ctx context.Context) (*Totals, error) {
	t := &Totals{ByStatus: map[string]int{}}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(cost_usd),0), COALESCE(SUM(input_tokens),0),
		COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0) FROM tasks`).
		Scan(&t.Tasks, &t.CostUSD, &t.InputTokens, &t.OutputTokens, &t.CacheRead)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		t.ByStatus[st] = n
	}
	return t, rows.Err()
}

// ---- approvals ----

const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
)

type Approval struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	ToolUseID  string          `json:"tool_use_id"`
	ToolName   string          `json:"tool_name"`
	Input      json.RawMessage `json:"input"`
	Reason     string          `json:"reason"`
	Status     string          `json:"status"`
	Note       string          `json:"note"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
}

func (s *Store) CreateApproval(ctx context.Context, a *Approval) error {
	ts := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO approvals (id, task_id, tool_use_id, tool_name, input, reason, status, note, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, a.ID, a.TaskID, a.ToolUseID, a.ToolName, string(a.Input), a.Reason, ApprovalPending, "", ts)
	if err != nil {
		return err
	}
	a.Status = ApprovalPending
	a.CreatedAt = time.UnixMilli(ts).UTC()
	return nil
}

const approvalCols = `id, task_id, tool_use_id, tool_name, input, reason, status, note, created_at, resolved_at`

func scanApproval(row interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var input string
	var created int64
	var resolved sql.NullInt64
	err := row.Scan(&a.ID, &a.TaskID, &a.ToolUseID, &a.ToolName, &input, &a.Reason, &a.Status, &a.Note, &created, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Input = json.RawMessage(input)
	a.CreatedAt = time.UnixMilli(created).UTC()
	if resolved.Valid {
		t := time.UnixMilli(resolved.Int64).UTC()
		a.ResolvedAt = &t
	}
	return &a, nil
}

func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
}

// ApprovalForToolUse returns the most recent approval for a tool call, if any.
func (s *Store) ApprovalForToolUse(ctx context.Context, taskID, toolUseID string) (*Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals
		WHERE task_id = ? AND tool_use_id = ? ORDER BY created_at DESC LIMIT 1`, taskID, toolUseID))
}

func (s *Store) ApprovalsForTask(ctx context.Context, taskID string) ([]*Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveApproval records a decision. It only changes pending approvals and
// reports whether it did.
func (s *Store) ResolveApproval(ctx context.Context, id, status, note string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, note = ?, resolved_at = ? WHERE id = ? AND status = ?`,
		status, note, now(), id, ApprovalPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
