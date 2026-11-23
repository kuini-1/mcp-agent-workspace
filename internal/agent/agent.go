// Package agent runs a task: it calls the model, executes the tools it asks
// for through MCP, applies the approval policy and loop guards, records
// cost, writes debug notes on failure and compacts context when it grows.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/kuini-1/mcp-agent-workspace/internal/journal"
	"github.com/kuini-1/mcp-agent-workspace/internal/mcphub"
	"github.com/kuini-1/mcp-agent-workspace/internal/policy"
	"github.com/kuini-1/mcp-agent-workspace/internal/store"
	"github.com/kuini-1/mcp-agent-workspace/internal/workspace"
)

// MessageClient is the one Messages API call the agent needs. The real
// implementation wraps the Anthropic SDK; tests and the demo use a script.
type MessageClient interface {
	New(ctx context.Context, params anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error)
}

// SDKClient adapts the Anthropic SDK client.
type SDKClient struct{ Client anthropic.Client }

func (c SDKClient) New(ctx context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	return c.Client.Beta.Messages.New(ctx, p)
}

type Config struct {
	Model           string
	Effort          string // low, medium, high, xhigh, max
	MaxTokens       int64
	MaxIterations   int    // model calls per task
	CompactAtTokens int64  // compact when a request reads this many tokens
	RepeatLimit     int    // identical calls before the loop guard trips
	FailRoundLimit  int    // consecutive all-failing tool rounds before stopping
	Fallbacks       bool   // server-side refusal fallbacks
	ShowThinking    bool   // ask for summarized thinking, shown in the timeline
	DataDir         string // journals live in DataDir/journal
	MCPServers      map[string]mcphub.ServerSpec
	DefaultBudget   float64 // USD
}

func DefaultConfig() Config {
	return Config{
		Model:           "claude-opus-5-5",
		Effort:          "high",
		MaxTokens:       16000,
		MaxIterations:   60,
		CompactAtTokens: 150_000,
		RepeatLimit:     3,
		FailRoundLimit:  5,
		Fallbacks:       true,
		ShowThinking:    true,
		DataDir:         "data",
		DefaultBudget:   2.00,
	}
}

type Runner struct {
	Store    *store.Store
	LLM      MessageClient
	Policy   *policy.Policy
	Approver Approver
	Cfg      Config
	// OnEvent, if set, is called for every timeline event (the CLI prints them).
	OnEvent func(store.Event)
	// ClientFor, if set, overrides LLM per task (the demo gives each task its own script).
	ClientFor func(taskID string) MessageClient
}

// NewTaskID returns a short random id like "t_3f9a1c2b7d".
func NewTaskID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "t_" + hex.EncodeToString(b)
}

func Title(prompt string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(prompt), "\n", 2)[0])
	if r := []rune(line); len(r) > 80 {
		line = string(r[:77]) + "..."
	}
	return line
}

func (r *Runner) JournalFor(t *store.Task) *journal.Journal {
	return journal.New(filepath.Join(r.Cfg.DataDir, "journal", t.ID+".md"), t.Title)
}

func (r *Runner) emit(ctx context.Context, taskID, typ string, data any) {
	ev, err := r.Store.AddEvent(context.WithoutCancel(ctx), taskID, typ, data)
	if err == nil && r.OnEvent != nil {
		r.OnEvent(*ev)
	}
}

// run holds the state of one Run call.
type run struct {
	*Runner
	task    *store.Task
	hub     *mcphub.Hub
	jr      *journal.Journal
	guard   *guard
	system  string
	tools   []anthropic.BetaToolUnionParam
	lastCtx int64 // tokens read by the most recent request
}

type outcome struct {
	done    bool
	blocked bool
	reason  string
	summary string
}

// Resume appends an optional human note and runs the task again.
func (r *Runner) Resume(ctx context.Context, taskID, note string) error {
	t, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status == store.StatusCompleted {
		return errors.New("task is already completed")
	}
	if note = strings.TrimSpace(note); note != "" {
		if err := appendUser(ctx, r.Store, t.ID, t.Epoch, []UserBlock{textBlock("Note from the user: " + note)}); err != nil {
			return err
		}
		r.emit(ctx, t.ID, "user_note", map[string]string{"text": note})
	}
	return r.Run(ctx, taskID)
}

// Run executes the task until it finishes, blocks, or ctx ends.
func (r *Runner) Run(ctx context.Context, taskID string) (err error) {
	t, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t.Status == store.StatusCompleted {
		return nil
	}
	if err := os.MkdirAll(t.Workdir, 0o755); err != nil {
		return err
	}

	jr := r.JournalFor(t)
	ws := &workspace.Workspace{Root: t.Workdir, Journal: jr}
	hub, err := mcphub.Connect(ctx, ws.Server(mcphub.Version), r.Cfg.MCPServers)
	if err != nil {
		r.block(ctx, t, jr, "could not start MCP servers: "+err.Error(), "")
		return err
	}
	defer hub.Close()

	rr := r
	if r.ClientFor != nil {
		cp := *r
		cp.LLM = r.ClientFor(t.ID)
		rr = &cp
	}
	x := &run{
		Runner: rr, task: t, hub: hub, jr: jr,
		guard:  newGuard(r.Cfg.RepeatLimit, r.Cfg.FailRoundLimit),
		system: systemPrompt(t.Workdir),
		tools:  buildTools(hub.Tools()),
	}

	if t.Iterations == 0 && t.Epoch == 0 {
		names := make([]string, 0, len(hub.Tools()))
		for _, tl := range hub.Tools() {
			names = append(names, tl.Name)
		}
		r.emit(ctx, t.ID, "task_started", map[string]any{
			"model": t.Model, "workdir": t.Workdir, "tools": names, "policy": string(r.Policy.Mode()),
		})
	} else {
		r.emit(ctx, t.ID, "task_resumed", map[string]any{"iterations": t.Iterations})
	}
	if err := r.Store.SetStatus(ctx, t.ID, store.StatusInProgress, ""); err != nil {
		return err
	}

	out, err := x.loop(ctx)
	if err != nil {
		reason := err.Error()
		if errors.Is(err, context.Canceled) {
			reason = "cancelled"
		}
		r.block(ctx, t, jr, reason, "")
		return err
	}
	if out.blocked {
		r.block(ctx, t, jr, out.reason, out.summary)
		return nil
	}
	r.complete(ctx, t, jr, out.summary)
	return nil
}

func (x *run) loop(ctx context.Context) (outcome, error) {
	for {
		if err := ctx.Err(); err != nil {
			return outcome{}, err
		}
		t, err := x.Store.GetTask(ctx, x.task.ID)
		if err != nil {
			return outcome{}, err
		}
		x.task = t
		hist, err := loadHistory(ctx, x.Store, t.ID, t.Epoch)
		if err != nil {
			return outcome{}, err
		}
		if len(hist) == 0 {
			if err := appendUser(ctx, x.Store, t.ID, t.Epoch, []UserBlock{textBlock(t.Prompt)}); err != nil {
				return outcome{}, err
			}
			continue
		}

		last := hist[len(hist)-1]
		if last.isAssistant() {
			if calls := pendingToolUses(last.assistant); len(calls) > 0 {
				out, err := x.runTools(ctx, calls)
				if err != nil || out.done || out.blocked {
					return out, err
				}
				continue
			}
			switch last.assistant.StopReason {
			case anthropic.BetaStopReasonEndTurn, anthropic.BetaStopReasonStopSequence:
				// The model ended its turn without asking for a tool: it considers the task done.
				return outcome{done: true, summary: finalText(last.assistant)}, nil
			}
			// A turn that stopped early (refusal, pause) is being resumed: nudge it on.
			if err := appendUser(ctx, x.Store, t.ID, t.Epoch, []UserBlock{textBlock("Continue.")}); err != nil {
				return outcome{}, err
			}
			continue
		}

		// The last message is ours: check the limits, then ask the model.
		if t.Iterations >= x.Cfg.MaxIterations {
			return outcome{blocked: true, reason: fmt.Sprintf("reached the limit of %d model calls", x.Cfg.MaxIterations)}, nil
		}
		if t.BudgetUSD > 0 && t.CostUSD >= t.BudgetUSD {
			return outcome{blocked: true, reason: fmt.Sprintf("spent $%.2f of the $%.2f budget", t.CostUSD, t.BudgetUSD)}, nil
		}
		if x.Cfg.CompactAtTokens > 0 && x.lastCtx >= x.Cfg.CompactAtTokens {
			if err := x.compact(ctx, hist); err != nil {
				return outcome{}, err
			}
			continue
		}

		resp, err := x.call(ctx, toParams(hist), "step", false)
		if err != nil {
			return outcome{}, err
		}
		if err := appendAssistant(ctx, x.Store, t.ID, t.Epoch, resp); err != nil {
			return outcome{}, err
		}
		if _, err := x.Store.IncIterations(ctx, t.ID); err != nil {
			return outcome{}, err
		}
		x.emitContent(ctx, resp)

		switch resp.StopReason {
		case anthropic.BetaStopReasonRefusal:
			reason := "the model declined the request"
			if c := string(resp.StopDetails.Category); c != "" {
				reason += " (" + c + ")"
			}
			return outcome{blocked: true, reason: reason}, nil
		case anthropic.BetaStopReasonModelContextWindowExceeded:
			return outcome{blocked: true, reason: "the conversation exceeded the model's context window; lower the compaction threshold"}, nil
		case anthropic.BetaStopReasonMaxTokens:
			if len(pendingToolUses(resp)) == 0 {
				msg := "Your reply hit the output limit. Continue, and keep each step smaller."
				if err := appendUser(ctx, x.Store, t.ID, t.Epoch, []UserBlock{textBlock(msg)}); err != nil {
					return outcome{}, err
				}
			}
		}
		// Tool calls (and plain end of turn) are handled at the top of the loop.
	}
}

// call sends one request and records its usage and cost.
func (x *run) call(ctx context.Context, msgs []anthropic.BetaMessageParam, purpose string, noTools bool) (*anthropic.BetaMessage, error) {
	p := anthropic.BetaMessageNewParams{
		Model:        anthropic.Model(x.task.Model),
		MaxTokens:    x.Cfg.MaxTokens,
		System:       []anthropic.BetaTextBlockParam{{Text: x.system}},
		Tools:        x.tools,
		Messages:     msgs,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(x.Cfg.Effort)},
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaThinkingBindingControls2026_08_01},
	}
	adaptive := anthropic.BetaThinkingConfigAdaptiveParam{
		// Our history is append-only, so a prefix mismatch can only be a bug: fail loudly.
		BlockBinding: anthropic.BetaThinkingBlockBindingParam{
			PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehaviorError,
		},
	}
	if x.Cfg.ShowThinking {
		adaptive.Display = anthropic.BetaThinkingConfigAdaptiveDisplaySummarized
	}
	p.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &adaptive}
	if noTools {
		p.ToolChoice = anthropic.BetaToolChoiceUnionParam{OfNone: &anthropic.BetaToolChoiceNoneParam{}}
	}
	if x.Cfg.Fallbacks {
		p.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}

	resp, err := x.LLM.New(ctx, p)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("model API error %d: %s", apiErr.StatusCode, apiErr.Error())
		}
		return nil, err
	}

	cost, known := Cost(string(resp.Model), resp.Usage)
	u := &store.Usage{
		TaskID: x.task.ID, Purpose: purpose, Model: string(resp.Model),
		InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
		CacheReadTokens: resp.Usage.CacheReadInputTokens, CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
		CostUSD: cost,
	}
	if err := x.Store.RecordUsage(ctx, u); err != nil {
		return nil, err
	}
	x.lastCtx = contextTokens(resp.Usage)
	x.emit(ctx, x.task.ID, "usage", map[string]any{
		"purpose": purpose, "model": u.Model, "input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_tokens": u.CacheReadTokens, "cache_write_tokens": u.CacheWriteTokens,
		"cost_usd": cost, "priced": known, "context_tokens": x.lastCtx, "stop_reason": string(resp.StopReason),
	})
	return resp, nil
}

func (x *run) emitContent(ctx context.Context, m *anthropic.BetaMessage) {
	for _, c := range m.Content {
		switch b := c.AsAny().(type) {
		case anthropic.BetaTextBlock:
			if strings.TrimSpace(b.Text) != "" {
				x.emit(ctx, x.task.ID, "assistant_text", map[string]string{"text": b.Text})
			}
		case anthropic.BetaThinkingBlock:
			if strings.TrimSpace(b.Thinking) != "" {
				x.emit(ctx, x.task.ID, "thinking", map[string]string{"text": b.Thinking})
			}
		case anthropic.BetaFallbackBlock:
			x.emit(ctx, x.task.ID, "fallback", map[string]string{"from": string(b.From.Model), "to": string(b.To.Model)})
		}
	}
}

// runTools executes every tool call of one assistant turn and appends all
// results as a single user message.
func (x *run) runTools(ctx context.Context, calls []anthropic.BetaToolUseBlock) (outcome, error) {
	var (
		blocks   []UserBlock
		out      outcome
		failures int
		tripped  error
	)
	for _, tu := range calls {
		input := json.RawMessage(tu.JSON.Input.Raw())
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		x.emit(ctx, x.task.ID, "tool_call", map[string]any{"id": tu.ID, "name": tu.Name, "input": input})

		result, isErr, err := x.runOne(ctx, tu.ID, tu.Name, input, &out, &tripped)
		if err != nil {
			return outcome{}, err
		}
		x.emit(ctx, x.task.ID, "tool_result", map[string]any{
			"id": tu.ID, "name": tu.Name, "is_error": isErr, "output": clip(result, 4000),
		})
		if isErr {
			failures++
			if n, jerr := x.jr.ToolFailure(tu.Name, string(input), result); jerr == nil {
				x.emit(ctx, x.task.ID, "debug_note", map[string]any{"attempt": n, "tool": tu.Name, "path": x.jr.Path()})
			}
		}
		blocks = append(blocks, UserBlock{Type: "tool_result", ToolUseID: tu.ID, Content: result, IsError: isErr})
	}

	if err := x.guard.round(len(calls), failures); err != nil && tripped == nil {
		tripped = err
	}
	if failures > 0 && tripped == nil && !out.done && !out.blocked {
		// Appended after the results and never removed: the history stays append-only.
		blocks = append(blocks, textBlock(failureReminder(failures)))
	}
	if err := appendUser(ctx, x.Store, x.task.ID, x.task.Epoch, blocks); err != nil {
		return outcome{}, err
	}
	if tripped != nil {
		x.emit(ctx, x.task.ID, "loop_guard", map[string]string{"reason": tripped.Error()})
		return outcome{blocked: true, reason: "loop guard: " + tripped.Error()}, nil
	}
	return out, nil
}

// runOne returns the tool_result text and whether it is an error. err is only
// for conditions that must stop the run (cancellation, storage failures).
func (x *run) runOne(ctx context.Context, id, name string, input json.RawMessage, out *outcome, tripped *error) (string, bool, error) {
	switch name {
	case toolFinish:
		var in struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(input, &in)
		out.done, out.summary = true, in.Summary
		return "Task marked as completed.", false, nil
	case toolBlocked:
		var in struct {
			Reason    string `json:"reason"`
			NextSteps string `json:"next_steps"`
		}
		_ = json.Unmarshal(input, &in)
		out.blocked, out.reason, out.summary = true, in.Reason, in.NextSteps
		return "Task marked as blocked.", false, nil
	}

	if err := x.guard.seen(name, input); err != nil {
		*tripped = err
		return "Stopped by the loop guard: " + err.Error() + ". The task is paused for a human to review.", true, nil
	}

	tool, ok := x.hub.Lookup(name)
	if !ok {
		return fmt.Sprintf("Unknown tool %q.", name), true, nil
	}
	decision, why := x.Policy.Check(policy.Tool{
		Name: tool.Remote, Server: tool.Server, ReadOnly: tool.ReadOnly, Destructive: tool.Destructive,
	}, input)

	switch decision {
	case policy.Deny:
		x.emit(ctx, x.task.ID, "policy_denied", map[string]string{"id": id, "name": name, "reason": why})
		return "Refused by policy: " + why + ". Use a different approach.", true, nil
	case policy.Ask:
		approved, note, err := x.approve(ctx, id, name, input, why)
		if err != nil {
			return "", false, err
		}
		if !approved {
			msg := "The user denied this action."
			if note != "" {
				msg += " Their note: " + note
			}
			return msg, true, nil
		}
	}

	res, isErr, err := x.hub.Call(ctx, name, input)
	if err != nil {
		return "", false, err
	}
	return res, isErr, nil
}

func (x *run) approve(ctx context.Context, toolUseID, name string, input json.RawMessage, reason string) (bool, string, error) {
	a, err := x.Store.ApprovalForToolUse(ctx, x.task.ID, toolUseID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a = &store.Approval{
			ID: "a_" + NewTaskID()[2:], TaskID: x.task.ID, ToolUseID: toolUseID,
			ToolName: name, Input: input, Reason: reason,
		}
		if err := x.Store.CreateApproval(ctx, a); err != nil {
			return false, "", err
		}
		x.emit(ctx, x.task.ID, "approval_requested", map[string]any{
			"approval_id": a.ID, "name": name, "input": input, "reason": reason,
		})
	case err != nil:
		return false, "", err
	}

	if a.Status == store.ApprovalPending {
		if err := x.Store.SetStatus(ctx, x.task.ID, store.StatusAwaitingApproval, ""); err != nil {
			return false, "", err
		}
		status, note, err := x.Approver.Wait(ctx, a)
		if err != nil {
			return false, "", err
		}
		a.Status, a.Note = status, note
		if err := x.Store.SetStatus(ctx, x.task.ID, store.StatusInProgress, ""); err != nil {
			return false, "", err
		}
		x.emit(ctx, x.task.ID, "approval_resolved", map[string]string{"approval_id": a.ID, "status": status, "note": note})
	}
	return a.Status == store.ApprovalApproved, a.Note, nil
}

// compact replaces the whole conversation with a summary written by the
// model ("simple compaction"). Nothing from the old conversation is replayed,
// so no thinking block outlives the history it was produced with.
func (x *run) compact(ctx context.Context, hist []entry) error {
	before := x.lastCtx
	msgs := append(toParams(hist), anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(compactionPrompt)))
	resp, err := x.call(ctx, msgs, "compaction", true)
	if err != nil {
		return fmt.Errorf("compaction: %w", err)
	}
	summary := strings.TrimSpace(finalText(resp))
	if summary == "" {
		return fmt.Errorf("compaction returned no summary (stop reason %s)", resp.StopReason)
	}
	epoch, err := x.Store.StartEpoch(ctx, x.task.ID)
	if err != nil {
		return err
	}
	msg := continuationMessage(x.task.Prompt, summary, epoch)
	if err := appendUser(ctx, x.Store, x.task.ID, epoch, []UserBlock{textBlock(msg)}); err != nil {
		return err
	}
	x.lastCtx = 0
	x.emit(ctx, x.task.ID, "compaction", map[string]any{
		"epoch": epoch, "tokens_before": before, "summary": summary,
	})
	if x.jr.Exists() {
		_ = x.jr.Section("Context compacted", fmt.Sprintf("At %d tokens. The agent continued from this summary:\n\n%s", before, summary))
	}
	return nil
}

func (r *Runner) complete(ctx context.Context, t *store.Task, jr *journal.Journal, summary string) {
	ctx = context.WithoutCancel(ctx)
	_ = r.Store.SetSummary(ctx, t.ID, summary)
	_ = r.Store.SetStatus(ctx, t.ID, store.StatusCompleted, "")
	if jr.Exists() {
		_ = jr.Section("Resolved", summary)
	}
	r.emit(ctx, t.ID, "task_completed", map[string]string{"summary": summary})
}

func (r *Runner) block(ctx context.Context, t *store.Task, jr *journal.Journal, reason, nextSteps string) {
	ctx = context.WithoutCancel(ctx)
	_ = r.Store.SetStatus(ctx, t.ID, store.StatusBlocked, reason)
	body := "Reason: " + reason
	if nextSteps != "" {
		body += "\n\nNext steps:\n\n" + nextSteps
		_ = r.Store.SetSummary(ctx, t.ID, nextSteps)
	}
	_ = jr.Section("Blocked", body)
	r.emit(ctx, t.ID, "task_blocked", map[string]string{"reason": reason, "next_steps": nextSteps})
}

// ---- tool definitions ----

const (
	toolFinish  = "finish_task"
	toolBlocked = "mark_blocked"
)

func buildTools(hubTools []mcphub.Tool) []anthropic.BetaToolUnionParam {
	defs := []anthropic.BetaToolParam{
		{
			Name:        toolFinish,
			Description: anthropic.String("Mark the task as completed. Call this once the work is done and verified."),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: map[string]any{
					"summary": map[string]any{"type": "string", "description": "What changed and how it was verified."},
				},
				Required: []string{"summary"},
			},
		},
		{
			Name:        toolBlocked,
			Description: anthropic.String("Stop and hand the task to a human because you cannot continue."),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: map[string]any{
					"reason":     map[string]any{"type": "string", "description": "Why you cannot continue."},
					"next_steps": map[string]any{"type": "string", "description": "What a human should do next."},
				},
				Required: []string{"reason", "next_steps"},
			},
		},
	}
	for _, t := range hubTools {
		def := anthropic.BetaToolParam{Name: t.Name, InputSchema: anthropic.BetaToolInputSchemaParam{Properties: t.Schema["properties"]}}
		if t.Description != "" {
			def.Description = anthropic.String(t.Description)
		}
		if req, ok := t.Schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					def.InputSchema.Required = append(def.InputSchema.Required, s)
				}
			}
		}
		if def.InputSchema.Properties == nil {
			def.InputSchema.Properties = map[string]any{}
		}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	out := make([]anthropic.BetaToolUnionParam, len(defs))
	for i := range defs {
		out[i] = anthropic.BetaToolUnionParam{OfTool: &defs[i]}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… (%d more bytes)", len(s)-n)
}
