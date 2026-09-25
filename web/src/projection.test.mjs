import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("./projection.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
}).outputText;
const { applyEvent } = await import(`data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`);

test("live replay matches an event-ordered snapshot across queued input", () => {
  const conversation = { conversation_id: "c", workspace: "/tmp/c", created_at: "2026-09-25T00:00:00Z" };
  const date = (second) => `2026-09-25T00:00:0${second}Z`;
  const input = (cursor, id, text) => ({
    cursor, type: "input.accepted", conversation_id: "c", inbound_message_id: id,
    input: { idempotency_key: id, parts: [{ type: "text", text }], source: { kind: "human", adapter: "http" }, target_agent_id: "default", agent_revision: "rev" },
    created_at: date(cursor),
  });
  const run = { id: "run-a", conversation_id: "c", inbound_message_id: "a", status: "running", started_at: date(2) };
  const events = [
    input(1, "a", "A"),
    { cursor: 2, type: "run.started", conversation_id: "c", run, created_at: date(2) },
    input(3, "b", "B"),
    { cursor: 4, type: "assistant.output.committed", conversation_id: "c", inbound_message_id: "a", run_id: "run-a", assistant_output: { id: "answer-a", parts: [{ type: "text", text: "answer A" }], final: true }, created_at: date(4) },
    { cursor: 5, type: "run.completed", conversation_id: "c", run: { ...run, status: "completed", completed_at: date(5) }, created_at: date(5) },
  ];
  const empty = { conversation, messages: [], submissions: [], tool_calls: [], environment_events: [], event_cursor: 0 };
  const live = events.reduce(applyEvent, empty);
  const snapshot = {
    ...empty, event_cursor: 5,
    messages: [
      { id: "a", conversation_id: "c", inbound_message_id: "a", role: "user", complete: true, parts: [{ type: "text", text: "A" }], created_at: date(1) },
      { id: "b", conversation_id: "c", inbound_message_id: "b", role: "user", complete: true, parts: [{ type: "text", text: "B" }], created_at: date(3) },
      { id: "answer-a", conversation_id: "c", inbound_message_id: "a", run_id: "run-a", role: "assistant", complete: true, final: true, parts: [{ type: "text", text: "answer A" }], created_at: date(4) },
    ],
    submissions: [
      { id: "a", conversation_id: "c", message_id: "a", agent_revision: "rev", status: "completed", accepted_at: date(1), error: undefined },
      { id: "b", conversation_id: "c", message_id: "b", agent_revision: "rev", status: "queued", accepted_at: date(3) },
    ],
    active_run: undefined,
  };
  assert.deepEqual(live, snapshot);
});

test("tool calls retain run-scoped identity during replay", () => {
  const created_at = "2026-09-25T00:00:00Z";
  const empty = { conversation: { conversation_id: "c" }, messages: [], submissions: [], tool_calls: [], event_cursor: 0 };
  const events = [
    { cursor: 1, type: "assistant.output.committed", conversation_id: "c", run_id: "r", inbound_message_id: "a", created_at,
      assistant_output: { id: "m", parts: [{ type: "tool_call", tool_call_id: "t", tool_kind: "shell", arguments: { command: "pwd" } }] } },
    { cursor: 2, type: "tool.outcome.recorded", conversation_id: "c", run_id: "r", inbound_message_id: "a", created_at,
      tool_outcome: { id: "result", tool_call_id: "t", tool_kind: "shell", status: "completed", result: { action_id: "t", ok: true, output: "/tmp" } } },
    { cursor: 3, type: "assistant.output.committed", conversation_id: "c", run_id: "r2", inbound_message_id: "b", created_at,
      assistant_output: { id: "m2", parts: [{ type: "tool_call", tool_call_id: "t", tool_kind: "shell", arguments: { command: "ls" } }] } },
    { cursor: 4, type: "tool.outcome.recorded", conversation_id: "c", run_id: "r2", inbound_message_id: "b", created_at,
      tool_outcome: { id: "result2", tool_call_id: "t", tool_kind: "shell", status: "failed", result: { action_id: "t", ok: false, error: "failed" } } },
  ];
  const live = events.reduce(applyEvent, empty);
  assert.equal(live.tool_calls.length, 2);
  assert.deepEqual(live.tool_calls[0], {
    id: "t", conversation_id: "c", run_id: "r", inbound_message_id: "a", kind: "shell",
    arguments: { command: "pwd" }, status: "completed", result: { action_id: "t", ok: true, output: "/tmp" }, updated_at: created_at,
  });
  assert.deepEqual(live.tool_calls[1], {
    id: "t", conversation_id: "c", run_id: "r2", inbound_message_id: "b", kind: "shell",
    arguments: { command: "ls" }, status: "failed", result: { action_id: "t", ok: false, error: "failed" }, updated_at: created_at,
  });
  assert.deepEqual(live.messages.map((message) => message.id), ["m", "result", "m2", "result2"]);
});
