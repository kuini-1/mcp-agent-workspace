import { useState } from "react";
import { usd, type Event } from "./api";

const s = (v: unknown) => (typeof v === "string" ? v : v === undefined ? "" : JSON.stringify(v));

export function Timeline({ events, prompt }: { events: Event[]; prompt: string }) {
  return (
    <ol className="timeline">
      <li className="ev ev-user">
        <div className="ev-kind">Task</div>
        <div className="ev-body prewrap">{prompt}</div>
      </li>
      {events.map((e) => <Item key={e.id} e={e} />)}
    </ol>
  );
}

function Item({ e }: { e: Event }) {
  const d = e.data;
  const time = new Date(e.time).toLocaleTimeString();
  switch (e.type) {
    case "assistant_text":
      return <Row kind="Agent" cls="ev-agent" time={time}><div className="prewrap">{s(d.text)}</div></Row>;
    case "thinking":
      return <Row kind="Thinking" cls="ev-thinking" time={time}><Collapsible text={s(d.text)} /></Row>;
    case "tool_call":
      return (
        <Row kind="Tool call" cls="ev-tool" time={time}>
          <span className="mono tool-name">{s(d.name)}</span>
          <ToolInput input={d.input} />
        </Row>
      );
    case "tool_result":
      return (
        <Row kind={d.is_error ? "Failed" : "Result"} cls={d.is_error ? "ev-fail" : "ev-result"} time={time}>
          <Collapsible text={s(d.output)} mono />
        </Row>
      );
    case "debug_note":
      return <Row kind="Debug note" cls="ev-note" time={time}>Failure {s(d.attempt)} recorded in the debug notes.</Row>;
    case "policy_denied":
      return <Row kind="Refused" cls="ev-fail" time={time}>{s(d.name)}: {s(d.reason)}</Row>;
    case "approval_requested":
      return <Row kind="Approval" cls="ev-approval" time={time}>Asked to run <span className="mono">{s(d.name)}</span> ({s(d.reason)})</Row>;
    case "approval_resolved":
      return (
        <Row kind="Approval" cls="ev-approval" time={time}>
          {d.status === "approved" ? "Approved" : "Denied"}{d.note ? `: ${s(d.note)}` : ""}
        </Row>
      );
    case "compaction":
      return (
        <Row kind="Compacted" cls="ev-note" time={time}>
          Context reset at {Number(d.tokens_before).toLocaleString()} tokens; continuing from a summary.
          <Collapsible text={s(d.summary)} />
        </Row>
      );
    case "loop_guard":
      return <Row kind="Loop guard" cls="ev-fail" time={time}>{s(d.reason)}</Row>;
    case "fallback":
      return <Row kind="Fallback" cls="ev-note" time={time}>{s(d.from)} declined; {s(d.to)} continued.</Row>;
    case "usage":
      return (
        <li className="ev ev-usage">
          <span className="hint mono">
            {Number(d.context_tokens).toLocaleString()} tokens read ({Number(d.cache_read_tokens).toLocaleString()} cached) ·{" "}
            {Number(d.output_tokens).toLocaleString()} out · {d.priced ? usd(Number(d.cost_usd)) : "unpriced"}
            {d.purpose === "compaction" ? " · compaction" : ""}
          </span>
        </li>
      );
    case "user_note":
      return <Row kind="You" cls="ev-user" time={time}><div className="prewrap">{s(d.text)}</div></Row>;
    case "task_started":
      return <Row kind="Started" cls="ev-sys" time={time}>Tools: {(d.tools as string[] | undefined)?.join(", ")}</Row>;
    case "task_resumed":
      return <Row kind="Resumed" cls="ev-sys" time={time}>Continuing after {s(d.iterations)} model calls.</Row>;
    case "task_completed":
      return <Row kind="Done" cls="ev-done" time={time}>{s(d.summary)}</Row>;
    case "task_blocked":
      return <Row kind="Blocked" cls="ev-fail" time={time}>{s(d.reason)}</Row>;
    default:
      return <Row kind={e.type} cls="ev-sys" time={time}><code>{JSON.stringify(d)}</code></Row>;
  }
}

function Row({ kind, cls, time, children }: { kind: string; cls: string; time: string; children: React.ReactNode }) {
  return (
    <li className={`ev ${cls}`}>
      <div className="ev-kind">{kind}<span className="ev-time">{time}</span></div>
      <div className="ev-body">{children}</div>
    </li>
  );
}

function ToolInput({ input }: { input: unknown }) {
  const obj = (input ?? {}) as Record<string, unknown>;
  if (typeof obj.command === "string") return <pre>{obj.command}</pre>;
  if (typeof obj.content === "string") {
    return (
      <>
        <span className="hint mono"> {s(obj.path)}</span>
        <Collapsible text={obj.content} mono />
      </>
    );
  }
  return <pre>{JSON.stringify(obj, null, 2)}</pre>;
}

function Collapsible({ text, mono }: { text: string; mono?: boolean }) {
  const [open, setOpen] = useState(false);
  const long = text.length > 400 || text.split("\n").length > 8;
  const shown = !long || open ? text : text.split("\n").slice(0, 8).join("\n").slice(0, 400) + "…";
  return (
    <div>
      {mono ? <pre>{shown}</pre> : <div className="prewrap">{shown}</div>}
      {long && <button className="link" onClick={() => setOpen(!open)}>{open ? "Show less" : "Show all"}</button>}
    </div>
  );
}
