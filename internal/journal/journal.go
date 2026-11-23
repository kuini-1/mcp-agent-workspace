// Package journal writes a task's debug notes: a Markdown file that records
// what was tried, what failed and what finally worked.
//
// The file is created lazily, on the first failure or the first note, so
// tasks that run cleanly don't leave empty files behind.
package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Note kinds the agent can record.
var Kinds = []string{"tried", "failed", "worked", "decision", "todo"}

type Journal struct {
	mu       sync.Mutex
	path     string
	title    string
	attempts int
	now      func() time.Time
}

// New returns a journal that will write to path. Nothing touches the disk
// until the first entry.
func New(path, title string) *Journal {
	return &Journal{path: path, title: title, now: time.Now}
}

func (j *Journal) Path() string { return j.path }

// Exists reports whether any entry has been written.
func (j *Journal) Exists() bool {
	_, err := os.Stat(j.path)
	return err == nil
}

func (j *Journal) Read() (string, error) {
	b, err := os.ReadFile(j.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

// Note appends an entry written by the agent.
func (j *Journal) Note(kind, text string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !valid(kind) {
		return fmt.Errorf("unknown note kind %q (use one of: %s)", kind, strings.Join(Kinds, ", "))
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("note text is empty")
	}
	return j.append(fmt.Sprintf("- **%s** (%s): %s\n", kind, j.stamp(), indentContinuation(text)))
}

// ToolFailure records a failed tool call automatically. It returns the
// attempt number so callers can show it.
func (j *Journal) ToolFailure(tool, input, errText string) (int, error) {
	j.mu.Lock()
	j.attempts++
	n := j.attempts
	j.mu.Unlock()
	entry := fmt.Sprintf("\n### Failure %d: `%s` (%s)\n\nInput:\n\n```json\n%s\n```\n\nError:\n\n```\n%s\n```\n\n",
		n, tool, j.stamp(), clip(input, 2000), clip(errText, 4000))
	return n, j.append(entry)
}

// Section appends a free-form section, used for compaction notices and the
// final blocked/resolved summary.
func (j *Journal) Section(heading, body string) error {
	return j.append(fmt.Sprintf("\n## %s (%s)\n\n%s\n\n", heading, j.stamp(), strings.TrimSpace(body)))
}

// Failures returns how many automatic failure entries were written.
func (j *Journal) Failures() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.attempts
}

func (j *Journal) append(s string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	fresh := false
	if _, err := os.Stat(j.path); os.IsNotExist(err) {
		fresh = true
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if fresh {
		header := fmt.Sprintf("# Debug notes: %s\n\nWritten while the agent worked. Failures are recorded automatically;"+
			" `tried` / `failed` / `worked` notes come from the agent.\n\n", j.title)
		if _, err := f.WriteString(header); err != nil {
			return err
		}
	}
	_, err = f.WriteString(s)
	return err
}

func (j *Journal) stamp() string { return j.now().UTC().Format("2006-01-02 15:04:05Z") }

func valid(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func indentContinuation(s string) string { return strings.ReplaceAll(s, "\n", "\n  ") }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… (%d more bytes)", len(s)-n)
}
