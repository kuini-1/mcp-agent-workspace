package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kuini-1/mcp-agent-workspace/internal/store"
)

// Approver waits for a human decision on a pending approval and returns
// store.ApprovalApproved or store.ApprovalDenied with an optional note.
type Approver interface {
	Wait(ctx context.Context, a *store.Approval) (status, note string, err error)
}

// FixedApprover answers every request the same way (tests, --yes).
type FixedApprover struct {
	Store   *store.Store
	Approve bool
	Note    string
}

func (f FixedApprover) Wait(ctx context.Context, a *store.Approval) (string, string, error) {
	status := store.ApprovalDenied
	if f.Approve {
		status = store.ApprovalApproved
	}
	if _, err := f.Store.ResolveApproval(ctx, a.ID, status, f.Note); err != nil {
		return "", "", err
	}
	return status, f.Note, nil
}

// PromptApprover asks on a terminal.
type PromptApprover struct {
	Store *store.Store
	In    io.Reader
	Out   io.Writer
	once  sync.Once
	rd    *bufio.Reader
}

func (p *PromptApprover) Wait(ctx context.Context, a *store.Approval) (string, string, error) {
	p.once.Do(func() { p.rd = bufio.NewReader(p.In) })
	fmt.Fprintf(p.Out, "\n  approval needed: %s\n  reason: %s\n  input: %s\n  allow? [y/N/note]: ", a.ToolName, a.Reason, clip(string(a.Input), 600))
	line, err := p.rd.ReadString('\n')
	if err != nil && line == "" {
		return "", "", err
	}
	line = strings.TrimSpace(line)
	status, note := store.ApprovalDenied, ""
	switch strings.ToLower(line) {
	case "y", "yes":
		status = store.ApprovalApproved
	case "", "n", "no":
	default:
		note = line // anything else is a denial with guidance for the agent
	}
	if _, err := p.Store.ResolveApproval(ctx, a.ID, status, note); err != nil {
		return "", "", err
	}
	return status, note, nil
}

// QueueApprover is used by the HTTP server: approvals are resolved from the
// dashboard, which calls Resolve. Waiters also poll the database, so a
// decision recorded by another process is picked up too.
type QueueApprover struct {
	Store *store.Store
	mu    sync.Mutex
	wait  map[string]chan struct{}
}

func NewQueueApprover(st *store.Store) *QueueApprover {
	return &QueueApprover{Store: st, wait: map[string]chan struct{}{}}
}

func (q *QueueApprover) Wait(ctx context.Context, a *store.Approval) (string, string, error) {
	q.mu.Lock()
	ch, ok := q.wait[a.ID]
	if !ok {
		ch = make(chan struct{}, 1)
		q.wait[a.ID] = ch
	}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.wait, a.ID)
		q.mu.Unlock()
	}()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		cur, err := q.Store.GetApproval(ctx, a.ID)
		if err != nil {
			return "", "", err
		}
		if cur.Status != store.ApprovalPending {
			return cur.Status, cur.Note, nil
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ch:
		case <-tick.C:
		}
	}
}

// Resolve records a decision and wakes the waiting run. It reports whether a
// run was waiting in this process.
func (q *QueueApprover) Resolve(ctx context.Context, id string, approve bool, note string) (bool, error) {
	status := store.ApprovalDenied
	if approve {
		status = store.ApprovalApproved
	}
	changed, err := q.Store.ResolveApproval(ctx, id, status, note)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, fmt.Errorf("approval %s is not pending", id)
	}
	q.mu.Lock()
	ch, ok := q.wait[id]
	q.mu.Unlock()
	if ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return ok, nil
}
