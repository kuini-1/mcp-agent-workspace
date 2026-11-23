// Package server exposes the task API and serves the dashboard.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kuini-1/mcp-agent-workspace/internal/agent"
	"github.com/kuini-1/mcp-agent-workspace/internal/store"
	"github.com/kuini-1/mcp-agent-workspace/internal/workspace"
)

type Server struct {
	Store         *store.Store
	Runner        *agent.Runner
	Approver      *agent.QueueApprover
	WorkspaceRoot string
	Static        fs.FS  // built dashboard; nil disables it
	Token         string // if set, API calls need "Authorization: Bearer <token>"
	MaxParallel   int
	DefaultPrompt string // pre-filled in the dashboard (demo mode)
	Log           *slog.Logger

	baseCtx context.Context
	mu      sync.Mutex
	running map[string]context.CancelFunc
	sem     chan struct{}
}

// Start prepares background state; ctx bounds every task run.
func (s *Server) Start(ctx context.Context) error {
	s.baseCtx = ctx
	s.running = map[string]context.CancelFunc{}
	if s.MaxParallel <= 0 {
		s.MaxParallel = 2
	}
	s.sem = make(chan struct{}, s.MaxParallel)
	if s.Log == nil {
		s.Log = slog.Default()
	}
	n, err := s.Store.MarkInterrupted(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.Log.Info("marked interrupted tasks as blocked", "count", n)
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("GET /api/tasks/{id}/events", s.events)
	mux.HandleFunc("POST /api/tasks/{id}/resume", s.resume)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/approvals/{id}", s.resolveApproval)
	if s.Static != nil {
		mux.Handle("/", spa(s.Static))
	}
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "missing or wrong API token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"model":          s.Runner.Cfg.Model,
		"policy":         string(s.Runner.Policy.Mode()),
		"default_budget": s.Runner.Cfg.DefaultBudget,
		"default_prompt": s.DefaultPrompt,
		"demo":           s.DefaultPrompt != "",
	})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Totals(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.ListTasks(r.Context(), 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tasks == nil {
		tasks = []*store.Task{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

type createReq struct {
	Prompt    string   `json:"prompt"`
	Workspace string   `json:"workspace"` // optional folder name under the workspace root
	BudgetUSD *float64 `json:"budget_usd"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}
	id := agent.NewTaskID()
	dir := id
	if req.Workspace != "" {
		dir = req.Workspace
	}
	workdir, err := workspace.Resolve(s.WorkspaceRoot, dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	budget := s.Runner.Cfg.DefaultBudget
	if req.BudgetUSD != nil {
		budget = *req.BudgetUSD
	}
	t := &store.Task{
		ID: id, Title: agent.Title(req.Prompt), Prompt: req.Prompt, Workdir: workdir,
		Model: s.Runner.Cfg.Model, BudgetUSD: budget,
	}
	if err := s.Store.CreateTask(r.Context(), t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.launch(t.ID, func(ctx context.Context) error { return s.Runner.Run(ctx, t.ID) })
	writeJSON(w, http.StatusCreated, t)
}

type taskDetail struct {
	Task      *store.Task       `json:"task"`
	Approvals []*store.Approval `json:"approvals"`
	Usage     []store.Usage     `json:"usage"`
	Journal   string            `json:"journal"`
	Running   bool              `json:"running"`
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.Store.GetTask(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := taskDetail{Task: t, Running: s.isRunning(t.ID)}
	if d.Approvals, err = s.Store.ApprovalsForTask(ctx, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d.Usage, err = s.Store.UsageForTask(ctx, t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.Journal, _ = s.Runner.JournalFor(t).Read()
	if d.Approvals == nil {
		d.Approvals = []*store.Approval{}
	}
	if d.Usage == nil {
		d.Usage = []store.Usage{}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	evs, err := s.Store.Events(r.Context(), r.PathValue("id"), after)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

type resumeReq struct {
	Note      string   `json:"note"`
	BudgetUSD *float64 `json:"budget_usd"`
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req resumeReq
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	t, err := s.Store.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t.Status == store.StatusCompleted {
		writeErr(w, http.StatusConflict, "task is already completed")
		return
	}
	if s.isRunning(id) {
		writeErr(w, http.StatusConflict, "task is already running")
		return
	}
	if req.BudgetUSD != nil {
		if err := s.Store.SetBudget(r.Context(), id, *req.BudgetUSD); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.launch(id, func(ctx context.Context) error { return s.Runner.Resume(ctx, id, req.Note) })
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "resuming"})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	cancel, ok := s.running[id]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusConflict, "task is not running")
		return
	}
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

type approvalReq struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note"`
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalReq
	if !readJSON(w, r, &req) {
		return
	}
	a, err := s.Store.GetApproval(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "approval not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	waiting, err := s.Approver.Resolve(r.Context(), a.ID, req.Approve, strings.TrimSpace(req.Note))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// After a restart nothing is waiting in this process: resume the task,
	// which re-runs the pending tool call with the recorded decision.
	if !waiting && !s.isRunning(a.TaskID) {
		s.launch(a.TaskID, func(ctx context.Context) error { return s.Runner.Run(ctx, a.TaskID) })
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "resolved", "approved": req.Approve})
}

func (s *Server) isRunning(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[id]
	return ok
}

// launch runs fn in the background, at most MaxParallel at a time.
func (s *Server) launch(id string, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.mu.Lock()
	if _, busy := s.running[id]; busy {
		s.mu.Unlock()
		cancel()
		return
	}
	s.running[id] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
		}()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return
		}
		if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("task stopped with an error", "task", id, "err", err)
		}
	}()
}

// spa serves the built dashboard and falls back to index.html.
func spa(static fs.FS) http.Handler {
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(static, p); err != nil {
			if _, err := fs.Stat(static, "index.html"); err != nil {
				http.Error(w, "dashboard not built: run `make web` (the API is still available under /api)", http.StatusNotFound)
				return
			}
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
