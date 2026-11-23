package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/kuini-1/mcp-agent-workspace/internal/store"
)

// The conversation is stored append-only, one row per message:
//   - assistant turns as the raw API response JSON, so thinking blocks and
//     their signatures come back byte-for-byte;
//   - user turns as a small list of text / tool_result blocks.
//
// Rebuilding the request from these rows gives the same prefix every time,
// which keeps the prompt cache warm and keeps thinking blocks valid (the API
// rejects replayed thinking whose earlier conversation was edited).

type UserBlock struct {
	Type      string `json:"type"` // "text" or "tool_result"
	Text      string `json:"text,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type entry struct {
	assistant *anthropic.BetaMessage
	user      []UserBlock
}

func (e entry) isAssistant() bool { return e.assistant != nil }

func textBlock(s string) UserBlock { return UserBlock{Type: "text", Text: s} }

func loadHistory(ctx context.Context, st *store.Store, taskID string, epoch int) ([]entry, error) {
	rows, err := st.Messages(ctx, taskID, epoch)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(rows))
	for _, r := range rows {
		switch r.Kind {
		case store.KindAssistant:
			var m anthropic.BetaMessage
			if err := json.Unmarshal(r.Body, &m); err != nil {
				return nil, fmt.Errorf("decode assistant message %d: %w", r.ID, err)
			}
			out = append(out, entry{assistant: &m})
		case store.KindUser:
			var blocks []UserBlock
			if err := json.Unmarshal(r.Body, &blocks); err != nil {
				return nil, fmt.Errorf("decode user message %d: %w", r.ID, err)
			}
			out = append(out, entry{user: blocks})
		default:
			return nil, fmt.Errorf("unknown message kind %q", r.Kind)
		}
	}
	return out, nil
}

func appendUser(ctx context.Context, st *store.Store, taskID string, epoch int, blocks []UserBlock) error {
	b, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	return st.AppendMessage(ctx, taskID, epoch, store.KindUser, b)
}

func appendAssistant(ctx context.Context, st *store.Store, taskID string, epoch int, m *anthropic.BetaMessage) error {
	raw := m.RawJSON()
	if raw == "" {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return st.AppendMessage(ctx, taskID, epoch, store.KindAssistant, []byte(raw))
}

func toParams(hist []entry) []anthropic.BetaMessageParam {
	out := make([]anthropic.BetaMessageParam, 0, len(hist))
	for _, e := range hist {
		if e.isAssistant() {
			out = append(out, e.assistant.ToParam())
			continue
		}
		blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(e.user))
		for _, b := range e.user {
			switch b.Type {
			case "tool_result":
				blocks = append(blocks, anthropic.NewBetaToolResultBlock(b.ToolUseID, b.Content, b.IsError))
			default:
				blocks = append(blocks, anthropic.NewBetaTextBlock(b.Text))
			}
		}
		out = append(out, anthropic.NewBetaUserMessage(blocks...))
	}
	return out
}

// pendingToolUses returns the tool calls in an assistant turn.
func pendingToolUses(m *anthropic.BetaMessage) []anthropic.BetaToolUseBlock {
	var out []anthropic.BetaToolUseBlock
	for _, c := range m.Content {
		if tu, ok := c.AsAny().(anthropic.BetaToolUseBlock); ok {
			out = append(out, tu)
		}
	}
	return out
}

func finalText(m *anthropic.BetaMessage) string {
	var s string
	for _, c := range m.Content {
		if t, ok := c.AsAny().(anthropic.BetaTextBlock); ok {
			if s != "" {
				s += "\n\n"
			}
			s += t.Text
		}
	}
	return s
}
