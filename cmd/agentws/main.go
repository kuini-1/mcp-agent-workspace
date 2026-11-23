// Command agentws runs the agent workspace: a web dashboard and API (serve),
// a single task in the terminal (run), or the workspace tools as a
// standalone MCP server over stdio (mcp).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuini-1/mcp-agent-workspace/internal/agent"
	"github.com/kuini-1/mcp-agent-workspace/internal/journal"
	"github.com/kuini-1/mcp-agent-workspace/internal/mcphub"
	"github.com/kuini-1/mcp-agent-workspace/internal/policy"
	"github.com/kuini-1/mcp-agent-workspace/internal/server"
	"github.com/kuini-1/mcp-agent-workspace/internal/store"
	"github.com/kuini-1/mcp-agent-workspace/internal/workspace"
	"github.com/kuini-1/mcp-agent-workspace/web"
)

const usage = `agentws: a self-hosted agent workspace built on MCP

Usage:
  agentws serve   [flags]            web dashboard and API
  agentws run     [flags] "<task>"   run one task in this terminal
  agentws resume  [flags] <id> [note]
  agentws tasks   [flags]            list tasks
  agentws show    [flags] <id>       print a task's timeline and debug notes
  agentws mcp     [-root dir]        serve the workspace tools over stdio (for Claude Desktop / Claude Code)

Run "agentws <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = cmdServe(ctx, args)
	case "run":
		err = cmdRun(ctx, args)
	case "resume":
		err = cmdResume(ctx, args)
	case "tasks":
		err = cmdTasks(ctx, args)
	case "show":
		err = cmdShow(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- shared flags ----

type common struct {
	dataDir    string
	configPath string
	model      string
	effort     string
	policyMode string
	compactAt  int64
	maxIter    int
	budget     float64
	fallbacks  bool
	demo       bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil {
		return v
	}
	return def
}

func (c *common) register(fs *flag.FlagSet) {
	d := agent.DefaultConfig()
	fs.StringVar(&c.dataDir, "data", env("AGENTWS_DATA", "data"), "directory for the database and debug notes")
	fs.StringVar(&c.configPath, "config", env("AGENTWS_CONFIG", "agentws.json"), "config file with MCP servers and policy rules (optional)")
	fs.StringVar(&c.model, "model", env("AGENTWS_MODEL", d.Model), "Claude model")
	fs.StringVar(&c.effort, "effort", env("AGENTWS_EFFORT", d.Effort), "effort: low, medium, high, xhigh or max")
	fs.StringVar(&c.policyMode, "approval", env("AGENTWS_APPROVAL", "safe"), "approval mode: safe, auto or readonly")
	fs.Int64Var(&c.compactAt, "compact-at", envInt("AGENTWS_COMPACT_AT", d.CompactAtTokens), "compact context when a request reads this many tokens")
	fs.IntVar(&c.maxIter, "max-iterations", int(envInt("AGENTWS_MAX_ITERATIONS", int64(d.MaxIterations))), "model calls per task")
	fs.Float64Var(&c.budget, "budget", envFloat("AGENTWS_BUDGET", d.DefaultBudget), "default spending cap per task, USD")
	fs.BoolVar(&c.fallbacks, "fallbacks", true, "let the API retry a refused request on a fallback model")
	fs.BoolVar(&c.demo, "demo", os.Getenv("AGENTWS_DEMO") == "1", "replay a canned conversation instead of calling the API (no key needed)")
}

type fileConfig struct {
	MCPServers map[string]mcphub.ServerSpec `json:"mcpServers"`
	Policy     struct {
		Mode    string            `json:"mode"`
		Servers map[string]string `json:"servers"`
	} `json:"policy"`
}

func (c *common) setup() (*store.Store, *agent.Runner, error) {
	var fc fileConfig
	if b, err := os.ReadFile(c.configPath); err == nil {
		if err := json.Unmarshal(b, &fc); err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", c.configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}

	switch c.effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return nil, nil, fmt.Errorf("unknown effort %q", c.effort)
	}
	modeStr := c.policyMode
	if fc.Policy.Mode != "" && modeStr == "safe" {
		modeStr = fc.Policy.Mode
	}
	mode, err := policy.ParseMode(modeStr)
	if err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(c.dataDir, "agentws.db"))
	if err != nil {
		return nil, nil, err
	}

	cfg := agent.DefaultConfig()
	cfg.Model, cfg.Effort = c.model, c.effort
	cfg.CompactAtTokens, cfg.MaxIterations = c.compactAt, c.maxIter
	cfg.DefaultBudget, cfg.Fallbacks = c.budget, c.fallbacks
	cfg.DataDir = c.dataDir
	cfg.MCPServers = fc.MCPServers

	r := &agent.Runner{
		Store:  st,
		Cfg:    cfg,
		Policy: policy.New(policy.Config{Mode: mode, ServerRules: fc.Policy.Servers}),
	}
	if c.demo {
		r.Cfg.Model = agent.DemoModel
		r.ClientFor = func(string) agent.MessageClient { return agent.DemoScript() }
	} else {
		// Credentials come from ANTHROPIC_API_KEY (or an `ant auth login` profile).
		r.LLM = agent.SDKClient{Client: anthropic.NewClient()}
	}
	return st, r, nil
}

// ---- serve ----

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c common
	c.register(fs)
	addr := fs.String("addr", env("AGENTWS_ADDR", "127.0.0.1:8080"), "listen address")
	wsRoot := fs.String("workspace", env("AGENTWS_WORKSPACE", "workspace"), "root folder for task workspaces")
	parallel := fs.Int("parallel", 2, "tasks that may run at the same time")
	fs.Parse(args)

	st, r, err := c.setup()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := os.MkdirAll(*wsRoot, 0o755); err != nil {
		return err
	}
	root, _ := filepath.Abs(*wsRoot)

	approver := agent.NewQueueApprover(st)
	r.Approver = approver
	srv := &server.Server{
		Store: st, Runner: r, Approver: approver, WorkspaceRoot: root,
		Static: web.Dist(), Token: os.Getenv("AGENTWS_TOKEN"), MaxParallel: *parallel,
	}
	if c.demo {
		srv.DefaultPrompt = agent.DemoPrompt
	}
	if err := srv.Start(ctx); err != nil {
		return err
	}

	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	mode := "model " + r.Cfg.Model
	if c.demo {
		mode = "DEMO mode (scripted replies, no API calls)"
	}
	slog.Info("agentws listening", "url", "http://"+*addr, "mode", mode, "approval", string(r.Policy.Mode()))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- run / resume ----

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	workdir := fs.String("workdir", ".", "folder the agent works in")
	yes := fs.Bool("yes", false, "approve every action that would ask (the deny list still applies)")
	fs.Parse(args)
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" && c.demo {
		prompt = agent.DemoPrompt
	}
	if prompt == "" {
		return errors.New(`give the task as an argument: agentws run "add a --verbose flag to main.go"`)
	}

	st, r, err := c.setup()
	if err != nil {
		return err
	}
	defer st.Close()
	abs, err := filepath.Abs(*workdir)
	if err != nil {
		return err
	}
	t := &store.Task{
		ID: agent.NewTaskID(), Title: agent.Title(prompt), Prompt: prompt, Workdir: abs,
		Model: r.Cfg.Model, BudgetUSD: c.budget,
	}
	if err := st.CreateTask(ctx, t); err != nil {
		return err
	}
	fmt.Printf("task %s in %s\n", t.ID, abs)
	return runInTerminal(ctx, st, r, *yes, func() error { return r.Run(ctx, t.ID) }, t.ID)
}

func cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	var c common
	c.register(fs)
	yes := fs.Bool("yes", false, "approve every action that would ask")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: agentws resume <id> [note]")
	}
	id, note := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	st, r, err := c.setup()
	if err != nil {
		return err
	}
	defer st.Close()
	return runInTerminal(ctx, st, r, *yes, func() error { return r.Resume(ctx, id, note) }, id)
}

func runInTerminal(ctx context.Context, st *store.Store, r *agent.Runner, yes bool, fn func() error, id string) error {
	if yes {
		r.Approver = agent.FixedApprover{Store: st, Approve: true}
	} else {
		r.Approver = &agent.PromptApprover{Store: st, In: os.Stdin, Out: os.Stdout}
	}
	r.OnEvent = printEvent
	if err := fn(); err != nil {
		return err
	}
	t, err := st.GetTask(context.WithoutCancel(ctx), id)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s  %s  $%.4f  %d in / %d out / %d cached tokens\n",
		strings.ToUpper(t.Status), t.ID, t.CostUSD, t.InputTokens, t.OutputTokens, t.CacheReadTokens)
	if j := r.JournalFor(t); j.Exists() {
		fmt.Println("debug notes:", j.Path())
	}
	return nil
}

func printEvent(e store.Event) {
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	s := func(k string) string { v, _ := d[k].(string); return v }
	switch e.Type {
	case "task_started":
		fmt.Printf("· started (model %v, approval %v)\n", d["model"], d["policy"])
	case "thinking":
		fmt.Printf("  thinking: %s\n", oneLine(s("text"), 160))
	case "assistant_text":
		fmt.Printf("\n%s\n", s("text"))
	case "tool_call":
		var in strings.Builder
		enc := json.NewEncoder(&in)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(d["input"])
		fmt.Printf("→ %s %s\n", s("name"), oneLine(in.String(), 140))
	case "tool_result":
		mark := "ok"
		if d["is_error"] == true {
			mark = "FAILED"
		}
		fmt.Printf("  %s: %s\n", mark, oneLine(s("output"), 160))
	case "policy_denied":
		fmt.Printf("  refused by policy: %s\n", s("reason"))
	case "debug_note":
		fmt.Printf("  debug note written (failure %v)\n", d["attempt"])
	case "compaction":
		fmt.Printf("· context compacted at %v tokens\n", d["tokens_before"])
	case "loop_guard":
		fmt.Printf("· loop guard: %s\n", s("reason"))
	case "fallback":
		fmt.Printf("· %s declined, %s continued\n", s("from"), s("to"))
	case "usage":
		fmt.Printf("  [%v in, %v cached, %v out, $%.4f]\n", d["input_tokens"], d["cache_read_tokens"], d["output_tokens"], d["cost_usd"])
	case "task_completed":
		fmt.Printf("\n✓ %s\n", s("summary"))
	case "task_blocked":
		fmt.Printf("\n✗ blocked: %s\n", s("reason"))
		if ns := s("next_steps"); ns != "" {
			fmt.Printf("  next steps: %s\n", ns)
		}
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---- tasks / show ----

func cmdTasks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	var c common
	c.register(fs)
	fs.Parse(args)
	st, _, err := c.setup()
	if err != nil {
		return err
	}
	defer st.Close()
	tasks, err := st.ListTasks(ctx, 50)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCOST\tCALLS\tCREATED\tTITLE")
	for _, t := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t$%.4f\t%d\t%s\t%s\n", t.ID, t.Status, t.CostUSD, t.Iterations,
			t.CreatedAt.Local().Format("Jan 02 15:04"), oneLine(t.Title, 60))
	}
	return tw.Flush()
}

func cmdShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	var c common
	c.register(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: agentws show <id>")
	}
	st, r, err := c.setup()
	if err != nil {
		return err
	}
	defer st.Close()
	t, err := st.GetTask(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s\n%s\n\n", t.ID, t.Status, t.Prompt)
	evs, err := st.Events(ctx, t.ID, 0)
	if err != nil {
		return err
	}
	for _, e := range evs {
		printEvent(e)
	}
	if notes, _ := r.JournalFor(t).Read(); notes != "" {
		fmt.Printf("\n----- debug notes -----\n%s", notes)
	}
	return nil
}

// ---- mcp ----

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	root := fs.String("root", ".", "workspace root the tools are confined to")
	fs.Parse(args)
	abs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if _, err := workspace.Resolve(abs, "."); err != nil {
		return err
	}
	ws := &workspace.Workspace{
		Root:    abs,
		Journal: journal.New(filepath.Join(abs, ".agentws", "journal.md"), filepath.Base(abs)),
	}
	// Policy is the MCP client's job in this mode (Claude Desktop / Claude Code ask before tool calls).
	return ws.Server(mcphub.Version).Run(ctx, &mcp.StdioTransport{})
}
