package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuini-1/mcp-agent-workspace/internal/agent"
	"github.com/kuini-1/mcp-agent-workspace/internal/policy"
	"github.com/kuini-1/mcp-agent-workspace/internal/store"
)

// TestDemoTaskThroughTheAPI drives the canned demo task over HTTP: create it,
// approve the two shell commands it asks for, and wait for completion.
func TestDemoTaskThroughTheAPI(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := agent.DefaultConfig()
	cfg.DataDir, cfg.Model = dir, agent.DemoModel
	approver := agent.NewQueueApprover(st)
	r := &agent.Runner{
		Store: st, Cfg: cfg, Approver: approver,
		Policy:    policy.New(policy.Config{Mode: policy.ModeSafe}),
		ClientFor: func(string) agent.MessageClient { return agent.DemoScript() },
	}
	srv := &Server{Store: st, Runner: r, Approver: approver, WorkspaceRoot: root, Token: "secret"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	call := func(method, path string, body any, out any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, &buf)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	// The token is enforced.
	if resp, _ := http.Get(ts.URL + "/api/tasks"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", resp.StatusCode)
	}
	// Workspaces can't escape the root.
	if code := call("POST", "/api/tasks", map[string]any{"prompt": "x", "workspace": "../etc"}, nil); code != http.StatusBadRequest {
		t.Fatalf("escaping workspace: status %d, want 400", code)
	}

	var task store.Task
	if code := call("POST", "/api/tasks", map[string]any{"prompt": agent.DemoPrompt}, &task); code != http.StatusCreated {
		t.Fatalf("create: status %d", code)
	}

	approved := map[string]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var d taskDetail
		call("GET", "/api/tasks/"+task.ID, nil, &d)
		if d.Task.Status == store.StatusCompleted {
			if d.Journal == "" {
				t.Error("expected debug notes after the failed run")
			}
			if len(approved) != 2 {
				t.Errorf("approved %d actions, want 2", len(approved))
			}
			return
		}
		if d.Task.Status == store.StatusBlocked {
			t.Fatalf("task blocked: %s", d.Task.BlockedReason)
		}
		for _, a := range d.Approvals {
			if a.Status == store.ApprovalPending && !approved[a.ID] {
				if code := call("POST", "/api/approvals/"+a.ID, map[string]any{"approve": true}, nil); code != http.StatusOK {
					t.Fatalf("approve: status %d", code)
				}
				approved[a.ID] = true
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish; status %s", d.Task.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
