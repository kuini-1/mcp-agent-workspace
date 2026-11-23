import { useCallback, useEffect, useState } from "react";
import { api, num, usd, type Config, type Task, type Totals } from "./api";
import { TaskList } from "./TaskList";
import { TaskView } from "./TaskView";

function useHashId(): [string | null, (id: string | null) => void] {
  const read = () => {
    const m = /^#\/t\/(.+)$/.exec(window.location.hash);
    return m ? decodeURIComponent(m[1]) : null;
  };
  const [id, setId] = useState<string | null>(read);
  useEffect(() => {
    const on = () => setId(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  const set = useCallback((next: string | null) => {
    window.location.hash = next ? `/t/${encodeURIComponent(next)}` : "";
  }, []);
  return [id, set];
}

export function App() {
  const [config, setConfig] = useState<Config | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [totals, setTotals] = useState<Totals | null>(null);
  const [error, setError] = useState("");
  const [selected, setSelected] = useHashId();

  const refresh = useCallback(async () => {
    try {
      const [t, s] = await Promise.all([api.tasks(), api.stats()]);
      setTasks(t);
      setTotals(s);
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, []);

  useEffect(() => {
    api.config().then(setConfig).catch((e) => setError(e.message));
    refresh();
    const t = setInterval(refresh, 2000);
    return () => clearInterval(t);
  }, [refresh]);

  const waiting = totals?.by_status["awaiting_approval"] ?? 0;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <span className="logo" aria-hidden>◆</span> Agent Workspace
          {config?.demo && <span className="pill demo">demo: scripted replies</span>}
        </div>
        {totals && (
          <div className="stats">
            <Stat label="Tasks" value={num(totals.tasks)} />
            <Stat label="Waiting on you" value={num(waiting)} highlight={waiting > 0} />
            <Stat label="Spent" value={usd(totals.cost_usd)} />
            <Stat label="Tokens in / out" value={`${compact(totals.input_tokens + totals.cache_read_tokens)} / ${compact(totals.output_tokens)}`} />
          </div>
        )}
      </header>
      {error && <div className="banner error">{error}</div>}
      <main className="layout">
        <aside className="sidebar">
          <NewTask config={config} onCreated={(t) => { refresh(); setSelected(t.id); }} />
          <TaskList tasks={tasks} selected={selected} onSelect={setSelected} />
        </aside>
        <section className="content">
          {selected ? (
            <TaskView key={selected} id={selected} onChange={refresh} />
          ) : (
            <Empty config={config} />
          )}
        </section>
      </main>
    </div>
  );
}

function compact(n: number) {
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}k`;
  return String(n);
}

function Stat({ label, value, highlight }: { label: string; value: string; highlight?: boolean }) {
  return (
    <div className={`stat${highlight ? " hot" : ""}`}>
      <span className="stat-value">{value}</span>
      <span className="stat-label">{label}</span>
    </div>
  );
}

function NewTask({ config, onCreated }: { config: Config | null; onCreated: (t: Task) => void }) {
  const [prompt, setPrompt] = useState("");
  const [budget, setBudget] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    if (config?.default_prompt) setPrompt(config.default_prompt);
  }, [config]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!prompt.trim()) return;
    setBusy(true);
    setErr("");
    try {
      const b = budget.trim() === "" ? config?.default_budget ?? 2 : Number(budget);
      const t = await api.create(prompt, b);
      setPrompt(config?.default_prompt ?? "");
      onCreated(t);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="card new-task" onSubmit={submit}>
      <label htmlFor="prompt">New task</label>
      <textarea
        id="prompt"
        rows={4}
        value={prompt}
        placeholder="Describe what the agent should do in its workspace…"
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit(e); }}
      />
      <div className="row">
        <input
          className="budget"
          inputMode="decimal"
          placeholder={`Budget $${(config?.default_budget ?? 2).toFixed(2)}`}
          value={budget}
          onChange={(e) => setBudget(e.target.value)}
          aria-label="Budget in USD"
        />
        <button type="submit" className="primary" disabled={busy || !prompt.trim()}>
          {busy ? "Starting…" : "Start"}
        </button>
      </div>
      {config && (
        <p className="hint">
          {config.model} · approval mode <b>{config.policy}</b>
        </p>
      )}
      {err && <p className="error-text">{err}</p>}
    </form>
  );
}

function Empty({ config }: { config: Config | null }) {
  return (
    <div className="empty">
      <h2>Pick a task, or start one</h2>
      <p>
        Each task runs in its own workspace folder. The agent reads and writes files and runs commands through MCP tools.
        Risky commands wait for your approval, dangerous ones are refused, and every failure is written to the task's
        debug notes.
      </p>
      {config?.demo && (
        <p className="hint">
          Demo mode replays a fixed conversation, so no API key is needed and nothing is billed. Approve the two shell
          commands it asks for to see it finish.
        </p>
      )}
    </div>
  );
}
