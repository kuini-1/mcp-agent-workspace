import { useCallback, useEffect, useRef, useState } from "react";
import { api, num, usd, type Approval, type Event, type TaskDetail } from "./api";
import { Markdown } from "./markdown";
import { StatusPill } from "./TaskList";
import { Timeline } from "./Timeline";

type Tab = "timeline" | "notes" | "usage";

export function TaskView({ id, onChange }: { id: string; onChange: () => void }) {
  const [detail, setDetail] = useState<TaskDetail | null>(null);
  const [events, setEvents] = useState<Event[]>([]);
  const [tab, setTab] = useState<Tab>("timeline");
  const [err, setErr] = useState("");
  const lastId = useRef(0);

  const load = useCallback(async () => {
    try {
      const [d, evs] = await Promise.all([api.task(id), api.events(id, lastId.current)]);
      setDetail(d);
      if (evs.length) {
        lastId.current = evs[evs.length - 1].id;
        setEvents((prev) => [...prev, ...evs]);
      }
      setErr("");
    } catch (e) {
      setErr((e as Error).message);
    }
  }, [id]);

  useEffect(() => {
    load();
    const t = setInterval(load, 1500);
    return () => clearInterval(t);
  }, [load]);

  if (!detail) return <p className="hint pad">{err || "Loading…"}</p>;
  const { task, approvals, usage, journal, running } = detail;
  const pending = approvals.filter((a) => a.status === "pending");
  const after = () => { load(); onChange(); };
  const spentPct = task.budget_usd > 0 ? Math.min(100, (task.cost_usd / task.budget_usd) * 100) : 0;

  return (
    <div className="task-view">
      <div className="task-head">
        <div>
          <h1>{task.title}</h1>
          <p className="hint mono">{task.id} · {task.model} · {task.workdir}</p>
        </div>
        <StatusPill status={task.status} />
      </div>

      <div className="metrics">
        <Metric label="Cost" value={`${usd(task.cost_usd)} of ${usd(task.budget_usd)}`}>
          <div className="meter" role="meter" aria-valuenow={spentPct} aria-valuemin={0} aria-valuemax={100}>
            <div style={{ width: `${spentPct}%` }} />
          </div>
        </Metric>
        <Metric label="Model calls" value={num(task.iterations)} />
        <Metric label="Input tokens" value={num(task.input_tokens + task.cache_read_tokens + task.cache_write_tokens)} />
        <Metric label="Served from cache" value={cacheRate(task.cache_read_tokens, task.input_tokens + task.cache_write_tokens)} />
        <Metric label="Output tokens" value={num(task.output_tokens)} />
        <Metric label="Compactions" value={num(task.epoch)} />
      </div>

      {task.status === "blocked" && (
        <div className="banner warn">
          <b>Blocked:</b> {task.blocked_reason}
          {task.summary && <p className="next-steps">{task.summary}</p>}
        </div>
      )}
      {task.status === "completed" && task.summary && (
        <div className="banner ok"><b>Done:</b> {task.summary}</div>
      )}

      {pending.map((a) => <ApprovalCard key={a.id} approval={a} onDone={after} />)}

      <Controls taskId={task.id} status={task.status} running={running} budget={task.budget_usd} onDone={after} />

      <nav className="tabs" role="tablist">
        <TabButton tab="timeline" cur={tab} set={setTab}>Timeline</TabButton>
        <TabButton tab="notes" cur={tab} set={setTab}>Debug notes{journal ? "" : " (none)"}</TabButton>
        <TabButton tab="usage" cur={tab} set={setTab}>Usage ({usage.length})</TabButton>
      </nav>

      {tab === "timeline" && <Timeline events={events} prompt={task.prompt} />}
      {tab === "notes" && (
        <div className="card notes">
          {journal ? <Markdown source={journal} /> : <p className="hint">No failures so far, so no debug notes were written.</p>}
        </div>
      )}
      {tab === "usage" && (
        <div className="card table-wrap">
          <table>
            <thead>
              <tr><th>#</th><th>Purpose</th><th>Model</th><th className="r">Input</th><th className="r">Cache read</th><th className="r">Cache write</th><th className="r">Output</th><th className="r">Cost</th></tr>
            </thead>
            <tbody>
              {usage.map((u, i) => (
                <tr key={u.id}>
                  <td>{i + 1}</td><td>{u.purpose}</td><td className="mono">{u.model}</td>
                  <td className="r">{num(u.input_tokens)}</td><td className="r">{num(u.cache_read_tokens)}</td>
                  <td className="r">{num(u.cache_write_tokens)}</td><td className="r">{num(u.output_tokens)}</td>
                  <td className="r">{usd(u.cost_usd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function cacheRate(read: number, uncached: number) {
  const total = read + uncached;
  return total === 0 ? "–" : `${Math.round((read / total) * 100)}%`;
}

function Metric({ label, value, children }: { label: string; value: string; children?: React.ReactNode }) {
  return (
    <div className="metric">
      <span className="metric-label">{label}</span>
      <span className="metric-value">{value}</span>
      {children}
    </div>
  );
}

function TabButton({ tab, cur, set, children }: { tab: Tab; cur: Tab; set: (t: Tab) => void; children: React.ReactNode }) {
  return (
    <button role="tab" aria-selected={tab === cur} className={tab === cur ? "active" : ""} onClick={() => set(tab)}>
      {children}
    </button>
  );
}

function ApprovalCard({ approval, onDone }: { approval: Approval; onDone: () => void }) {
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const decide = async (approve: boolean) => {
    setBusy(true);
    try {
      await api.resolve(approval.id, approve, note);
      onDone();
    } finally {
      setBusy(false);
    }
  };
  const input = approval.input as Record<string, unknown>;
  const command = typeof input?.command === "string" ? input.command : null;
  return (
    <div className="card approval">
      <div className="approval-head">
        <b>Approval needed:</b> <span className="mono">{approval.tool_name}</span>
        <span className="hint"> · {approval.reason}</span>
      </div>
      <pre>{command ?? JSON.stringify(approval.input, null, 2)}</pre>
      <div className="row">
        <input placeholder="Optional note for the agent (sent with a denial)" value={note} onChange={(e) => setNote(e.target.value)} />
        <button className="primary" disabled={busy} onClick={() => decide(true)}>Approve</button>
        <button className="danger" disabled={busy} onClick={() => decide(false)}>Deny</button>
      </div>
    </div>
  );
}

function Controls({ taskId, status, running, budget, onDone }: {
  taskId: string; status: string; running: boolean; budget: number; onDone: () => void;
}) {
  const [note, setNote] = useState("");
  const [newBudget, setNewBudget] = useState("");
  const [err, setErr] = useState("");
  if (running) {
    return (
      <div className="controls">
        <button onClick={() => api.cancel(taskId).then(onDone).catch((e) => setErr(e.message))}>Stop task</button>
        {err && <span className="error-text">{err}</span>}
      </div>
    );
  }
  if (status !== "blocked") return null;
  const resume = async () => {
    try {
      const b = newBudget.trim() === "" ? undefined : Number(newBudget);
      await api.resume(taskId, note, b);
      setNote("");
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <div className="card controls">
      <input placeholder="Guidance for the agent (optional)" value={note} onChange={(e) => setNote(e.target.value)} />
      <input className="budget" inputMode="decimal" placeholder={`Budget $${budget.toFixed(2)}`} value={newBudget} onChange={(e) => setNewBudget(e.target.value)} aria-label="New budget in USD" />
      <button className="primary" onClick={resume}>Resume</button>
      {err && <span className="error-text">{err}</span>}
    </div>
  );
}
