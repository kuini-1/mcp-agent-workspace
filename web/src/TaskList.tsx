import { ago, usd, type Status, type Task } from "./api";

export const STATUS_LABEL: Record<Status, string> = {
  todo: "To do",
  in_progress: "Running",
  awaiting_approval: "Needs approval",
  blocked: "Blocked",
  completed: "Done",
};

export function StatusPill({ status }: { status: Status }) {
  return <span className={`pill status-${status}`}>{STATUS_LABEL[status] ?? status}</span>;
}

export function TaskList({
  tasks,
  selected,
  onSelect,
}: {
  tasks: Task[];
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  if (tasks.length === 0) return <p className="hint pad">No tasks yet.</p>;
  return (
    <ul className="task-list">
      {tasks.map((t) => (
        <li key={t.id}>
          <button className={`task-row${t.id === selected ? " active" : ""}`} onClick={() => onSelect(t.id)}>
            <span className="task-title">{t.title}</span>
            <span className="task-meta">
              <StatusPill status={t.status} />
              <span>{usd(t.cost_usd)}</span>
              <span>{ago(t.created_at)}</span>
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}
