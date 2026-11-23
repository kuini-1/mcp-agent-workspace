// Package workspace is an MCP server exposing file and shell tools that are
// confined to one directory, plus a journal_note tool for debug notes.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuini-1/mcp-agent-workspace/internal/journal"
)

const (
	maxReadBytes   = 200_000
	maxOutputBytes = 30_000
	defaultTimeout = 60 * time.Second
	maxTimeout     = 10 * time.Minute
)

type Workspace struct {
	Root    string
	Journal *journal.Journal // may be nil
}

// Server builds the MCP server. Tool annotations tell clients which tools
// only read, which the agent's policy uses to skip approval.
func (w *Workspace) Server(version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agentws-workspace", Version: version}, nil)
	yes, no := true, false

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a text file in the workspace. Returns numbered lines. Use offset/limit (1-based lines) for large files.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, w.readFile)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_dir",
		Description: "List files under a workspace directory (recursive, up to 300 entries, skips .git and node_modules).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, w.listDir)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or overwrite a file in the workspace with the full new content. Parent directories are created.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
	}, w.writeFile)

	mcp.AddTool(s, &mcp.Tool{
		Name: "run_shell",
		Description: "Run a shell command (sh -c) with the workspace as working directory. Returns exit code, stdout and stderr " +
			"(truncated). Some commands need human approval and some are refused.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes},
	}, w.runShell)

	mcp.AddTool(s, &mcp.Tool{
		Name: "journal_note",
		Description: "Add an entry to this task's debug notes. kind is one of: tried, failed, worked, decision, todo. " +
			"Record what you tried and why it failed before retrying, and what worked once something does.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &no},
	}, w.journalNote)

	return s
}

type ReadFileInput struct {
	Path   string `json:"path" jsonschema:"file path, relative to the workspace root"`
	Offset int    `json:"offset,omitempty" jsonschema:"first line to return (1-based)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of lines to return"`
}

func (w *Workspace) readFile(_ context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, any, error) {
	p, err := Resolve(w.Root, in.Path)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory; use list_dir", in.Path)
	}
	if info.Size() > maxReadBytes && in.Limit == 0 {
		return nil, nil, fmt.Errorf("%s is %d bytes; read it in parts with offset and limit", in.Path, info.Size())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(b), "\n")
	start := 1
	if in.Offset > 0 {
		start = in.Offset
	}
	end := len(lines)
	if in.Limit > 0 && start-1+in.Limit < end {
		end = start - 1 + in.Limit
	}
	var sb strings.Builder
	for i := start - 1; i < end && i < len(lines); i++ {
		fmt.Fprintf(&sb, "%5d  %s\n", i+1, lines[i])
	}
	if sb.Len() == 0 {
		sb.WriteString("(empty)")
	}
	return text(sb.String()), nil, nil
}

type ListDirInput struct {
	Path string `json:"path,omitempty" jsonschema:"directory, relative to the workspace root (default: root)"`
}

func (w *Workspace) listDir(_ context.Context, _ *mcp.CallToolRequest, in ListDirInput) (*mcp.CallToolResult, any, error) {
	dir, err := Resolve(w.Root, in.Path)
	if err != nil {
		return nil, nil, err
	}
	var out []string
	const limit = 300
	errStop := errors.New("stop")
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		if len(out) >= limit {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, nil, err
	}
	sort.Strings(out)
	if len(out) == 0 {
		return text("(empty directory)"), nil, nil
	}
	s := strings.Join(out, "\n")
	if len(out) >= limit {
		s += fmt.Sprintf("\n… stopped at %d entries", limit)
	}
	return text(s), nil, nil
}

type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"file path, relative to the workspace root"`
	Content string `json:"content" jsonschema:"the complete new file content"`
}

func (w *Workspace) writeFile(_ context.Context, _ *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, any, error) {
	p, err := Resolve(w.Root, in.Path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(in.Content))), nil, nil
}

type RunShellInput struct {
	Command    string `json:"command" jsonschema:"the shell command to run"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"timeout in seconds (default 60, max 600)"`
}

func (w *Workspace) runShell(ctx context.Context, _ *mcp.CallToolRequest, in RunShellInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Command) == "" {
		return nil, nil, errors.New("command is empty")
	}
	timeout := defaultTimeout
	if in.TimeoutSec > 0 {
		timeout = min(time.Duration(in.TimeoutSec)*time.Second, maxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	root, err := Resolve(w.Root, ".")
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	case runErr != nil && ctx.Err() == context.DeadlineExceeded:
		code = -1
	case runErr != nil:
		return nil, nil, runErr
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "exit code: %d\n", code)
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(&sb, "timed out after %s\n", timeout)
	}
	if stdout.Len() > 0 {
		fmt.Fprintf(&sb, "--- stdout ---\n%s\n", truncate(stdout.String(), maxOutputBytes))
	}
	if stderr.Len() > 0 {
		fmt.Fprintf(&sb, "--- stderr ---\n%s\n", truncate(stderr.String(), maxOutputBytes))
	}
	res := text(sb.String())
	// A non-zero exit is a failed attempt from the agent's point of view.
	res.IsError = code != 0
	return res, nil, nil
}

type JournalNoteInput struct {
	Kind string `json:"kind" jsonschema:"one of: tried, failed, worked, decision, todo"`
	Text string `json:"text" jsonschema:"what happened and why, in one or two sentences"`
}

func (w *Workspace) journalNote(_ context.Context, _ *mcp.CallToolRequest, in JournalNoteInput) (*mcp.CallToolResult, any, error) {
	if w.Journal == nil {
		return nil, nil, errors.New("no journal configured")
	}
	if err := w.Journal.Note(in.Kind, in.Text); err != nil {
		return nil, nil, err
	}
	return text("noted"), nil, nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	half := n / 2
	return s[:half] + fmt.Sprintf("\n… (%d bytes omitted) …\n", len(s)-n) + s[len(s)-half:]
}
