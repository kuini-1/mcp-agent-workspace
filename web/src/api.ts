export type Status = "todo" | "in_progress" | "awaiting_approval" | "blocked" | "completed";

export interface Task {
  id: string;
  title: string;
  prompt: string;
  status: Status;
  workdir: string;
  model: string;
  budget_usd: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  iterations: number;
  epoch: number;
  blocked_reason: string;
  summary: string;
  created_at: string;
  updated_at: string;
}

export interface Approval {
  id: string;
  task_id: string;
  tool_use_id: string;
  tool_name: string;
  input: unknown;
  reason: string;
  status: "pending" | "approved" | "denied";
  note: string;
  created_at: string;
  resolved_at?: string;
}

export interface Usage {
  id: number;
  time: string;
  purpose: "step" | "compaction";
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_usd: number;
}

export interface TaskDetail {
  task: Task;
  approvals: Approval[];
  usage: Usage[];
  journal: string;
  running: boolean;
}

export interface Event {
  id: number;
  time: string;
  type: string;
  data: Record<string, unknown>;
}

export interface Totals {
  tasks: number;
  by_status: Record<string, number>;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
}

export interface Config {
  model: string;
  policy: string;
  default_budget: number;
  default_prompt: string;
  demo: boolean;
}

const TOKEN_KEY = "agentws.token";

function token(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const t = token();
  if (t) headers["Authorization"] = `Bearer ${t}`;

  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  if (res.status === 401) {
    const entered = window.prompt("This server needs an API token (AGENTWS_TOKEN):");
    if (entered) {
      try {
        localStorage.setItem(TOKEN_KEY, entered.trim());
      } catch {
        /* storage unavailable: the prompt shows again next time */
      }
      return call(method, path, body);
    }
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`);
  return data as T;
}

export const api = {
  config: () => call<Config>("GET", "/api/config"),
  stats: () => call<Totals>("GET", "/api/stats"),
  tasks: () => call<Task[]>("GET", "/api/tasks"),
  task: (id: string) => call<TaskDetail>("GET", `/api/tasks/${id}`),
  events: (id: string, after: number) => call<Event[]>("GET", `/api/tasks/${id}/events?after=${after}`),
  create: (prompt: string, budget_usd: number) => call<Task>("POST", "/api/tasks", { prompt, budget_usd }),
  resume: (id: string, note: string, budget_usd?: number) =>
    call("POST", `/api/tasks/${id}/resume`, { note, budget_usd }),
  cancel: (id: string) => call("POST", `/api/tasks/${id}/cancel`),
  resolve: (id: string, approve: boolean, note: string) => call("POST", `/api/approvals/${id}`, { approve, note }),
};

export const usd = (n: number) => (n < 0.01 && n > 0 ? `$${n.toFixed(4)}` : `$${n.toFixed(2)}`);
export const num = (n: number) => n.toLocaleString("en-US");
export const ago = (iso: string) => {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return `${Math.round(s)}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
};
