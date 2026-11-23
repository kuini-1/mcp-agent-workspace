package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// Scripted is a MessageClient that replays canned responses in order and
// records every request. It never calls the API. Tests use it to drive the
// agent loop deterministically, and the demo mode uses it so the dashboard
// can be tried without an API key.
type Scripted struct {
	mu       sync.Mutex
	Replies  []string // raw Messages API response JSON
	Requests []anthropic.BetaMessageNewParams
	next     int
}

func (s *Scripted) New(_ context.Context, p anthropic.BetaMessageNewParams) (*anthropic.BetaMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, p)
	if s.next >= len(s.Replies) {
		return nil, fmt.Errorf("scripted client: no reply left for request %d", len(s.Requests))
	}
	raw := s.Replies[s.next]
	s.next++
	var m anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("scripted client: bad reply %d: %w", s.next, err)
	}
	return &m, nil
}

// Block is one content block of a scripted reply.
type Block = map[string]any

func Text(s string) Block { return Block{"type": "text", "text": s} }

func ToolUse(id, name string, input any) Block {
	return Block{"type": "tool_use", "id": id, "name": name, "input": input}
}

// ReplyUsage sets token counts on a scripted reply.
type ReplyUsage struct {
	Input, Output, CacheRead, CacheWrite int64
}

var replySeq int

// Reply builds a response JSON string.
func Reply(model, stopReason string, u ReplyUsage, blocks ...Block) string {
	replySeq++
	b, _ := json.Marshal(map[string]any{
		"id":          fmt.Sprintf("msg_scripted_%03d", replySeq),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"stop_reason": stopReason,
		"content":     blocks,
		"usage": map[string]any{
			"input_tokens":                u.Input,
			"output_tokens":               u.Output,
			"cache_read_input_tokens":     u.CacheRead,
			"cache_creation_input_tokens": u.CacheWrite,
		},
	})
	return string(b)
}
