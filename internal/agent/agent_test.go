package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/kuini-1/mcp-agent-workspace/internal/policy"
	"github.com/kuini-1/mcp-agent-workspace/internal/store"
)

const model = "claude-opus-5-5"

var small = ReplyUsage{Input: 1500, Output: 200}

type harness struct {
	t      *testing.T
	runner *Runner
	script *Scripted
	task   *store.Task
}

func newHarness(t *testing.T, replies []string, mutate func(*Config, *store.Task)) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := DefaultConfig()
	cfg.DataDir = dir
	task := &store.Task{
		ID: NewTaskID(), Title: "test", Prompt: "do the thing", Workdir: filepath.Join(dir, "ws"),
		Model: model, BudgetUSD: 5,
	}
	if mutate != nil {
		mutate(&cfg, task)
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	script := &Scripted{Replies: replies}
	r := &Runner{
		Store: st, LLM: script, Cfg: cfg,
		Policy:   policy.New(policy.Config{Mode: policy.ModeSafe}),
		Approver: FixedApprover{Store: st, Approve: true},
	}
	return &harness{t: t, runner: r, script: script, task: task}
}

func (h *harness) run() *store.Task {
	h.t.Helper()
	if err := h.runner.Run(context.Background(), h.task.ID); err != nil {
		h.t.Fatalf("Run: %v", err)
	}
	return h.reload()
}

func (h *harness) reload() *store.Task {
	h.t.Helper()
	t, err := h.runner.Store.GetTask(context.Background(), h.task.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) events(typ string) []store.Event {
	h.t.Helper()
	all, err := h.runner.Store.Events(context.Background(), h.task.ID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Event
	for _, e := range all {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestFailureWritesDebugNotesThenCompletes(t *testing.T) {
	h := newHarness(t, []string{
		Reply(model, "tool_use", small, Text("Reading the config."), ToolUse("tu_1", "read_file", Block{"path": "config.json"})),
		Reply(model, "tool_use", small,
			ToolUse("tu_2", "journal_note", Block{"kind": "failed", "text": "config.json does not exist yet"}),
			ToolUse("tu_3", "write_file", Block{"path": "config.json", "content": `{"debug": true}`})),
		Reply(model, "tool_use", small, ToolUse("tu_4", "read_file", Block{"path": "config.json"})),
		Reply(model, "tool_use", small, ToolUse("tu_5", "journal_note", Block{"kind": "worked", "text": "created config.json"})),
		Reply(model, "tool_use", small, ToolUse("tu_6", "finish_task", Block{"summary": "Created config.json and read it back."})),
	}, nil)

	task := h.run()
	if task.Status != store.StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", task.Status, task.BlockedReason)
	}
	if task.Summary != "Created config.json and read it back." {
		t.Errorf("summary = %q", task.Summary)
	}

	notes, err := h.runner.JournalFor(task).Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Failure 1: `read_file`", "config.json does not exist yet", "**worked**", "## Resolved"} {
		if !strings.Contains(notes, want) {
			t.Errorf("debug notes missing %q:\n%s", want, notes)
		}
	}
	if got := len(h.events("debug_note")); got != 1 {
		t.Errorf("debug_note events = %d, want 1", got)
	}

	// The failure reminder follows the tool result in the next request.
	second := h.script.Requests[1].Messages
	lastMsg, _ := json.Marshal(second[len(second)-1])
	if !strings.Contains(string(lastMsg), "journal_note") {
		t.Errorf("expected a failure reminder after the failed tool result, got %s", lastMsg)
	}

	// 5 calls of 1500 input + 200 output tokens at $4/$20 per million.
	want := 5 * (1500*4.0 + 200*20.0) / 1e6
	if diff := task.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %.6f, want %.6f", task.CostUSD, want)
	}
	if task.InputTokens != 7500 || task.OutputTokens != 1000 {
		t.Errorf("tokens = %d in / %d out", task.InputTokens, task.OutputTokens)
	}
}

func TestEveryRequestExtendsThePreviousOne(t *testing.T) {
	h := newHarness(t, []string{
		Reply(model, "tool_use", small, Block{"type": "thinking", "thinking": "list first", "signature": "sig1"},
			ToolUse("tu_1", "list_dir", Block{})),
		Reply(model, "tool_use", small, ToolUse("tu_2", "write_file", Block{"path": "a.txt", "content": "a"})),
		Reply(model, "tool_use", small, ToolUse("tu_3", "read_file", Block{"path": "missing.txt"})),
		Reply(model, "end_turn", small, Text("Done.")),
	}, nil)
	h.run()

	reqs := h.script.Requests
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1], reqs[i]
		if mustJSON(t, prev.System) != mustJSON(t, cur.System) {
			t.Errorf("request %d changed the system prompt", i)
		}
		if mustJSON(t, prev.Tools) != mustJSON(t, cur.Tools) {
			t.Errorf("request %d changed the tool list", i)
		}
		if len(cur.Messages) <= len(prev.Messages) {
			t.Fatalf("request %d did not grow the history", i)
		}
		for j := range prev.Messages {
			if mustJSON(t, prev.Messages[j]) != mustJSON(t, cur.Messages[j]) {
				t.Errorf("request %d rewrote message %d", i, j)
			}
		}
	}
	// The thinking block and its signature are replayed unchanged.
	if !strings.Contains(mustJSON(t, reqs[3].Messages[1]), `"signature":"sig1"`) {
		t.Errorf("thinking block not replayed: %s", mustJSON(t, reqs[3].Messages[1]))
	}
}

func TestLoopGuardStopsRepeatedCalls(t *testing.T) {
	same := func(id string) string {
		return Reply(model, "tool_use", small, ToolUse(id, "read_file", Block{"path": "nope.txt"}))
	}
	h := newHarness(t, []string{same("tu_1"), same("tu_2"), same("tu_3"), same("tu_4")}, nil)
	task := h.run()
	if task.Status != store.StatusBlocked || !strings.Contains(task.BlockedReason, "loop guard") {
		t.Fatalf("status = %s (%s), want blocked by loop guard", task.Status, task.BlockedReason)
	}
	if n := len(h.script.Requests); n != 3 {
		t.Errorf("model calls = %d, want 3", n)
	}
}

func TestApprovalDeniedIsReportedToTheModel(t *testing.T) {
	h := newHarness(t, []string{
		Reply(model, "tool_use", small, ToolUse("tu_1", "run_shell", Block{"command": "go run ."})),
		Reply(model, "tool_use", small, ToolUse("tu_2", "finish_task", Block{"summary": "stopped"})),
	}, nil)
	h.runner.Approver = FixedApprover{Store: h.runner.Store, Approve: false, Note: "use go test instead"}
	h.run()

	last := mustJSON(t, h.script.Requests[1].Messages[2])
	if !strings.Contains(last, "denied") || !strings.Contains(last, "use go test instead") {
		t.Errorf("denial not passed to the model: %s", last)
	}
	approvals, _ := h.runner.Store.ApprovalsForTask(context.Background(), h.task.ID)
	if len(approvals) != 1 || approvals[0].Status != store.ApprovalDenied {
		t.Fatalf("approvals = %+v", approvals)
	}
}

func TestPolicyRefusesDangerousCommand(t *testing.T) {
	h := newHarness(t, []string{
		Reply(model, "tool_use", small, ToolUse("tu_1", "run_shell", Block{"command": "sudo rm -rf /"})),
		Reply(model, "end_turn", small, Text("ok")),
	}, nil)
	h.runner.Approver = FixedApprover{Store: h.runner.Store, Approve: true} // must not even be asked
	h.run()
	if len(h.events("policy_denied")) != 1 {
		t.Error("expected a policy_denied event")
	}
	if len(h.events("approval_requested")) != 0 {
		t.Error("a denied command must not reach the approver")
	}
}

func TestCompactionStartsFreshFromSummary(t *testing.T) {
	big := ReplyUsage{Input: 900, CacheRead: 9000, Output: 300}
	h := newHarness(t, []string{
		Reply(model, "tool_use", big, Block{"type": "thinking", "thinking": "", "signature": "sig_old"},
			ToolUse("tu_1", "write_file", Block{"path": "notes.md", "content": "step one"})),
		Reply(model, "end_turn", small, Text("Goal: do the thing. Wrote notes.md. Next: verify it.")), // the summary
		Reply(model, "tool_use", small, ToolUse("tu_2", "read_file", Block{"path": "notes.md"})),
		Reply(model, "tool_use", small, ToolUse("tu_3", "finish_task", Block{"summary": "verified"})),
	}, func(c *Config, _ *store.Task) { c.CompactAtTokens = 5000 })

	task := h.run()
	if task.Status != store.StatusCompleted {
		t.Fatalf("status = %s (%s)", task.Status, task.BlockedReason)
	}
	if task.Epoch != 1 {
		t.Errorf("epoch = %d, want 1", task.Epoch)
	}

	summaryReq := h.script.Requests[1]
	if summaryReq.ToolChoice.OfNone == nil {
		t.Error("compaction request should disable tool use")
	}
	after := h.script.Requests[2]
	if len(after.Messages) != 1 {
		t.Fatalf("after compaction the history should be one message, got %d", len(after.Messages))
	}
	body := mustJSON(t, after.Messages[0])
	if !strings.Contains(body, "Wrote notes.md") || !strings.Contains(body, "do the thing") {
		t.Errorf("continuation message lacks the summary or task: %s", body)
	}
	if strings.Contains(body, "sig_old") {
		t.Error("old thinking blocks must not be replayed after compaction")
	}
	if len(h.events("compaction")) != 1 {
		t.Error("expected one compaction event")
	}
}

func TestBudgetStopsTheTask(t *testing.T) {
	pricey := ReplyUsage{Input: 1000, Output: 50_000} // about $1.00 per call at $4/$20
	h := newHarness(t, []string{
		Reply(model, "tool_use", pricey, ToolUse("tu_1", "list_dir", Block{})),
		Reply(model, "tool_use", pricey, ToolUse("tu_2", "list_dir", Block{"path": "."})),
	}, func(_ *Config, task *store.Task) { task.BudgetUSD = 1.50 })
	task := h.run()
	if task.Status != store.StatusBlocked || !strings.Contains(task.BlockedReason, "budget") {
		t.Fatalf("status = %s (%s), want blocked on budget", task.Status, task.BlockedReason)
	}
	if n := len(h.script.Requests); n != 2 {
		t.Errorf("model calls = %d, want 2", n)
	}
}

func TestRefusalBlocksAndResumeContinues(t *testing.T) {
	h := newHarness(t, []string{
		Reply(model, "refusal", small),
		Reply(model, "tool_use", small, ToolUse("tu_1", "finish_task", Block{"summary": "done after resume"})),
	}, nil)
	task := h.run()
	if task.Status != store.StatusBlocked || !strings.Contains(task.BlockedReason, "declined") {
		t.Fatalf("status = %s (%s)", task.Status, task.BlockedReason)
	}
	if err := h.runner.Resume(context.Background(), task.ID, ""); err != nil {
		t.Fatal(err)
	}
	if task = h.reload(); task.Status != store.StatusCompleted {
		t.Fatalf("after resume: status = %s (%s)", task.Status, task.BlockedReason)
	}
}

func TestRequestShape(t *testing.T) {
	h := newHarness(t, []string{Reply(model, "end_turn", small, Text("hi"))}, nil)
	h.run()
	p := h.script.Requests[0]
	if p.Thinking.OfAdaptive == nil {
		t.Fatal("adaptive thinking not set")
	}
	if p.Thinking.OfAdaptive.BlockBinding.PrefixMismatchBehavior != anthropic.BetaThinkingPrefixMismatchBehaviorError {
		t.Error("prefix mismatch behavior should be explicit")
	}
	if p.OutputConfig.Effort != "high" {
		t.Errorf("effort = %q", p.OutputConfig.Effort)
	}
	var names []string
	for _, tl := range p.Tools {
		names = append(names, tl.OfTool.Name)
	}
	want := "finish_task journal_note list_dir mark_blocked read_file run_shell write_file"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("tools = %s\nwant    %s", got, want)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMain(m *testing.M) {
	// Shell tools run with sh; make sure tests don't inherit a weird cwd.
	if err := os.Chdir(os.TempDir()); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
