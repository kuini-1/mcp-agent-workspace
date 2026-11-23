// Package mcphub connects to the MCP servers a task uses and presents their
// tools as one flat, stable list.
//
// The built-in workspace server runs in-process over an in-memory transport,
// so it goes through exactly the same client path as external servers.
// External tools are namespaced as "<server>__<tool>".
package mcphub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerSpec is an external MCP server launched over stdio, in the same shape
// Claude Desktop and Claude Code use in their config files.
type ServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type Tool struct {
	Name        string // name shown to the model
	Server      string // "" for the built-in workspace server
	Remote      string // tool name on its server
	Description string
	Schema      map[string]any // JSON Schema of the input
	ReadOnly    bool
	Destructive bool
}

type Hub struct {
	sessions map[string]*mcp.ClientSession
	tools    []Tool
	byName   map[string]Tool
}

const Version = "0.1.0"

// Connect starts the built-in server (when builtin is non-nil) and every
// external server, then lists their tools. It fails if any server fails, so
// a task never starts with a silently missing toolset.
func Connect(ctx context.Context, builtin *mcp.Server, external map[string]ServerSpec) (*Hub, error) {
	h := &Hub{sessions: map[string]*mcp.ClientSession{}, byName: map[string]Tool{}}
	client := mcp.NewClient(&mcp.Implementation{Name: "agentws", Version: Version}, nil)

	if builtin != nil {
		st, ct := mcp.NewInMemoryTransports()
		if _, err := builtin.Connect(ctx, st, nil); err != nil {
			return nil, fmt.Errorf("start workspace server: %w", err)
		}
		cs, err := client.Connect(ctx, ct, nil)
		if err != nil {
			return nil, fmt.Errorf("connect workspace server: %w", err)
		}
		h.sessions[""] = cs
	}

	names := make([]string, 0, len(external))
	for name := range external {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.Contains(name, "__") || name == "" {
			h.Close()
			return nil, fmt.Errorf("invalid MCP server name %q", name)
		}
		spec := external[name]
		cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
		cmd.Env = os.Environ()
		for k, v := range spec.Env {
			cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
		}
		cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("connect MCP server %q: %w", name, err)
		}
		h.sessions[name] = cs
	}

	for server, cs := range h.sessions {
		for t, err := range cs.Tools(ctx, nil) {
			if err != nil {
				h.Close()
				return nil, fmt.Errorf("list tools of %q: %w", server, err)
			}
			tool := Tool{
				Name:        t.Name,
				Server:      server,
				Remote:      t.Name,
				Description: t.Description,
				Schema:      schemaMap(t.InputSchema),
			}
			if server != "" {
				tool.Name = server + "__" + t.Name
			}
			if a := t.Annotations; a != nil {
				tool.ReadOnly = a.ReadOnlyHint
				// MCP treats a missing destructiveHint as "may be destructive".
				tool.Destructive = a.DestructiveHint == nil || *a.DestructiveHint
			} else {
				tool.Destructive = true
			}
			h.tools = append(h.tools, tool)
			h.byName[tool.Name] = tool
		}
	}
	// Sorted, so the tool list sent to the model is identical on every request.
	sort.Slice(h.tools, func(i, j int) bool { return h.tools[i].Name < h.tools[j].Name })
	return h, nil
}

func (h *Hub) Tools() []Tool { return h.tools }

func (h *Hub) Lookup(name string) (Tool, bool) {
	t, ok := h.byName[name]
	return t, ok
}

// Call runs a tool. A tool that reports an error (IsError) returns its
// message with isErr=true; err is reserved for transport problems.
func (h *Hub) Call(ctx context.Context, name string, input json.RawMessage) (out string, isErr bool, err error) {
	t, ok := h.byName[name]
	if !ok {
		return fmt.Sprintf("unknown tool %q", name), true, nil
	}
	cs := h.sessions[t.Server]
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return "tool input is not a JSON object: " + err.Error(), true, nil
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: t.Remote, Arguments: args})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", true, err
		}
		// Protocol-level errors (bad arguments, unknown tool) are the model's to fix.
		return err.Error(), true, nil
	}
	return contentText(res), res.IsError, nil
}

func (h *Hub) Close() {
	for _, cs := range h.sessions {
		cs.Close()
	}
}

func contentText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		default:
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n")
}

func schemaMap(s any) map[string]any {
	if s == nil {
		return map[string]any{"type": "object"}
	}
	if m, ok := s.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(s)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{"type": "object"}
	}
	return m
}
