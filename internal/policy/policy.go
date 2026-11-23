// Package policy decides whether a tool call may run on its own, needs a
// human's approval, or is refused outright.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	default:
		return "deny"
	}
}

// Mode sets how much the agent may do without asking.
type Mode string

const (
	// ModeSafe runs read-only work and known-safe commands; everything else asks.
	ModeSafe Mode = "safe"
	// ModeAuto runs everything except the deny list.
	ModeAuto Mode = "auto"
	// ModeReadOnly refuses anything that could change files or run programs.
	ModeReadOnly Mode = "readonly"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeSafe, ModeAuto, ModeReadOnly:
		return Mode(s), nil
	case "":
		return ModeSafe, nil
	}
	return "", fmt.Errorf("unknown approval mode %q (safe, auto, readonly)", s)
}

// Tool describes what the policy needs to know about a tool.
type Tool struct {
	Name        string
	Server      string // "" for built-in workspace tools
	ReadOnly    bool
	Destructive bool
}

// ServerRule overrides the default for every tool from one MCP server.
// Values: "allow", "ask", "deny".
type Config struct {
	Mode        Mode
	ServerRules map[string]string
}

type Policy struct {
	cfg Config
}

func New(cfg Config) *Policy {
	if cfg.Mode == "" {
		cfg.Mode = ModeSafe
	}
	return &Policy{cfg: cfg}
}

func (p *Policy) Mode() Mode { return p.cfg.Mode }

// Check returns the decision for one call and a short reason a human can read.
func (p *Policy) Check(t Tool, input json.RawMessage) (Decision, string) {
	if t.Server != "" {
		if rule, ok := p.cfg.ServerRules[t.Server]; ok {
			switch rule {
			case "allow":
				return Allow, "server rule: allow"
			case "deny":
				return Deny, "server rule: deny"
			case "ask":
				return Ask, "server rule: ask"
			}
		}
	}

	if t.Name == "run_shell" && t.Server == "" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		d, why := Shell(in.Command)
		switch {
		case d == Deny:
			return Deny, why
		case p.cfg.Mode == ModeReadOnly && d != Allow:
			return Deny, "read-only mode: " + why
		case p.cfg.Mode == ModeAuto:
			return Allow, "auto mode"
		}
		return d, why
	}

	if t.ReadOnly {
		return Allow, "read-only tool"
	}
	switch p.cfg.Mode {
	case ModeReadOnly:
		return Deny, "read-only mode: tool can change state"
	case ModeAuto:
		return Allow, "auto mode"
	}
	// Built-in file writes are confined to the workspace, so they don't need a human.
	if t.Server == "" && !t.Destructive {
		return Allow, "workspace write"
	}
	return Ask, "tool can change state"
}

// ---- shell commands ----

var denyPatterns = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(^|[\s;&|(])sudo\s`), "uses sudo"},
	{regexp.MustCompile(`(^|[\s;&|(])su\s`), "switches user"},
	{regexp.MustCompile(`rm\s+(-[a-zA-Z]*[rf][a-zA-Z]*\s+)+(/|~|\$HOME|\*)(\s|$|/\*)`), "recursive delete of / or home"},
	{regexp.MustCompile(`(^|[\s;&|])mkfs`), "formats a filesystem"},
	{regexp.MustCompile(`\bdd\s+.*of=/dev/`), "writes to a device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|disk)`), "writes to a device"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`(curl|wget)[^|]*\|\s*(sudo\s+)?(sh|bash|zsh|python3?)\b`), "pipes a download into a shell"},
	{regexp.MustCompile(`chmod\s+(-R\s+)?[0-7]*777\s+/`), "opens permissions on /"},
	{regexp.MustCompile(`(^|[\s;&|])(shutdown|reboot|halt|poweroff)\b`), "powers off the machine"},
	{regexp.MustCompile(`git\s+push\s+.*(--force|-f)\b`), "force-pushes"},
	{regexp.MustCompile(`(^|[\s;&|])(ssh|scp)\s`), "opens a remote connection"},
}

// Read-only or build/test commands that are safe to run without asking.
var safeCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "rg": true, "pwd": true,
	"echo": true, "which": true, "tree": true, "stat": true, "diff": true, "sort": true, "uniq": true,
	"cut": true, "true": true, "file": true, "du": true, "date": true, "env": false,
}

var safeSubcommands = map[string][]string{
	"go":   {"build", "test", "vet", "version", "list", "doc", "env"},
	"git":  {"status", "diff", "log", "show", "branch", "rev-parse"},
	"npm":  {"test", "ls", "run test", "run build", "run lint"},
	"node": {"--version", "-v"},
	"find": {""}, // allowed unless it deletes or executes (checked below)
}

var splitOps = regexp.MustCompile(`&&|\|\||;|\|`)

// Shell classifies a shell command.
func Shell(cmd string) (Decision, string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Deny, "empty command"
	}
	for _, p := range denyPatterns {
		if p.re.MatchString(cmd) {
			return Deny, "blocked: " + p.why
		}
	}
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return Ask, "uses command substitution"
	}
	if regexp.MustCompile(`(^|[^0-9&])>{1,2}`).MatchString(stripQuoted(cmd)) {
		return Ask, "redirects output to a file"
	}
	for _, seg := range splitOps.Split(cmd, -1) {
		if !safeSegment(strings.TrimSpace(seg)) {
			return Ask, fmt.Sprintf("not on the safe list: %q", firstWords(seg, 3))
		}
	}
	return Allow, "safe command"
}

func safeSegment(seg string) bool {
	if seg == "" {
		return true
	}
	fields := strings.Fields(seg)
	name := fields[0]
	if safeCommands[name] {
		return true
	}
	subs, ok := safeSubcommands[name]
	if !ok {
		return false
	}
	if name == "find" {
		return !strings.Contains(seg, "-delete") && !strings.Contains(seg, "-exec") && !strings.Contains(seg, "-ok")
	}
	rest := strings.Join(fields[1:], " ")
	for _, s := range subs {
		if rest == s || strings.HasPrefix(rest, s+" ") {
			return true
		}
	}
	return false
}

// stripQuoted removes quoted strings so "echo 'a > b'" isn't read as a redirect.
func stripQuoted(s string) string {
	return regexp.MustCompile(`'[^']*'|"[^"]*"`).ReplaceAllString(s, "''")
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}
