// Tiny dependency-free TypeScript/Node SDK for pons tool_provider/v1.
// stdout is protocol-only; application logs belong on stderr.

export type JSONValue = null | boolean | number | string | JSONValue[] | { [key: string]: JSONValue };

export interface Action {
  id: string;
  kind: string;
  args: Record<string, unknown>;
  danger?: Record<string, unknown>;
}

export interface ToolResult {
  action_id?: string;
  ok: boolean;
  output?: string;
  error?: string;
  exit_code?: number;
  kind?: string;
  payload?: JSONValue;
}

export interface Tool {
  kind: string;
  description: string;
  input_schema: Record<string, unknown>;
  // The signal is canceled by $/cancelRequest and during shutdown.
  handler: (action: Action, signal: AbortSignal) => ToolResult | Promise<ToolResult>;
}

export interface Server {
  name: string;
  version?: string;
  tools: Tool[];
  // Omitted means serial; explicit zero means unbounded.
  maxConcurrency?: number;
  maxFrameBytes?: number;
  shutdownTimeoutMs?: number;
}

const RUNTIME_PROTOCOL = 1;
const TOOL_PROVIDER = "tool_provider";
const TOOL_PROVIDER_VERSION = 1;
const byteLength = (value: string) => new TextEncoder().encode(value).byteLength;

const errorMessage = (value: unknown): string => value instanceof Error ? value.message : String(value);

function validateSchema(value: unknown, path: string, root = false): void {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${path} must be a JSON Schema object`);
  }
  const schema = value as Record<string, any>;
  const allowed = new Set(["type", "properties", "required", "description", "enum", "items", "additionalProperties"]);
  for (const key of Object.keys(schema)) {
    if (!allowed.has(key)) throw new Error(`${path} uses unsupported keyword ${JSON.stringify(key)}`);
  }
  const types = new Set(["object", "string", "integer", "number", "boolean", "array", "null"]);
  if (typeof schema.type !== "string" || !types.has(schema.type)) throw new Error(`${path}.type is unsupported`);
  if (root && schema.type !== "object") throw new Error("input_schema root type must be object");
  if (schema.description !== undefined && typeof schema.description !== "string") throw new Error(`${path}.description must be a string`);
  if (schema.enum !== undefined && (!Array.isArray(schema.enum) || schema.enum.length === 0)) throw new Error(`${path}.enum must be non-empty`);

  if (schema.properties !== undefined) {
    if (schema.type !== "object" || schema.properties === null || typeof schema.properties !== "object" || Array.isArray(schema.properties)) {
      throw new Error(`${path}.properties must be an object schema map`);
    }
    for (const [name, child] of Object.entries(schema.properties)) validateSchema(child, `${path}.properties[${JSON.stringify(name)}]`);
  }
  if (schema.required !== undefined) {
    if (schema.type !== "object" || !Array.isArray(schema.required) || schema.required.some((name: unknown) => typeof name !== "string")) {
      throw new Error(`${path}.required must be an array of strings`);
    }
    const properties = schema.properties ?? {};
    for (const name of schema.required) if (!(name in properties)) throw new Error(`${path}.required references unknown property ${JSON.stringify(name)}`);
  }
  if (schema.items !== undefined) {
    if (schema.type !== "array") throw new Error(`${path}.items is only valid for arrays`);
    validateSchema(schema.items, `${path}.items`);
  } else if (schema.type === "array") {
    throw new Error(`${path}.items is required for arrays`);
  }
  if (schema.additionalProperties !== undefined && (schema.type !== "object" || schema.additionalProperties !== false)) {
    throw new Error(`${path}.additionalProperties must be false for objects`);
  }
}

type Waiter = {
  signal: AbortSignal;
  canceled: boolean;
  abort: () => void;
  resolve: (release: () => void) => void;
  reject: (reason: Error) => void;
};

class Semaphore {
  private available: number;
  private readonly waiters: Waiter[] = [];

  constructor(private readonly limit: number) {
    this.available = limit;
  }

  async acquire(signal: AbortSignal): Promise<() => void> {
    if (this.limit === 0) return () => {};
    if (signal.aborted) throw new Error("canceled");
    if (this.available > 0) {
      this.available--;
      return () => this.release();
    }
    return new Promise<() => void>((resolve, reject) => {
      let waiter!: Waiter;
      const abort = () => {
        waiter.canceled = true;
        signal.removeEventListener("abort", abort);
        reject(new Error("canceled"));
      };
      waiter = { signal, canceled: false, abort, resolve, reject };
      signal.addEventListener("abort", abort, { once: true });
      this.waiters.push(waiter);
    });
  }

  private release(): void {
    while (this.waiters.length > 0) {
      const next = this.waiters.shift()!;
      if (next.canceled) continue;
      next.signal.removeEventListener("abort", next.abort);
      next.resolve(() => this.release());
      return;
    }
    this.available++;
  }
}

/** Serve one persistent plugin over Node stdin/stdout. */
export async function serve(server: Server, input?: any, output?: any): Promise<void> {
  const nodeProcess = (globalThis as any).process;
  input ??= nodeProcess.stdin;
  output ??= nodeProcess.stdout;
  if (!server.name) throw new Error("name is required");
  if (server.maxConcurrency !== undefined && (!Number.isInteger(server.maxConcurrency) || server.maxConcurrency < 0)) throw new Error("maxConcurrency must be a non-negative integer");

  const maxFrameBytes = server.maxFrameBytes ?? 1024 * 1024;
  const shutdownTimeoutMs = server.shutdownTimeoutMs ?? 2000;
  const maxConcurrency = server.maxConcurrency ?? 1;
  const tools = new Map<string, Tool>();
  for (const tool of server.tools) {
    if (!tool.kind || tool.kind === "finish") throw new Error(`invalid tool kind ${JSON.stringify(tool.kind)}`);
    if (!tool.description || typeof tool.handler !== "function") throw new Error(`invalid tool ${JSON.stringify(tool.kind)}`);
    if (tools.has(tool.kind)) throw new Error(`duplicate tool kind ${JSON.stringify(tool.kind)}`);
    validateSchema(tool.input_schema, `$tool[${JSON.stringify(tool.kind)}]`, true);
    tools.set(tool.kind, tool);
  }

  // Node supplies this built-in module; the ignore keeps the SDK dependency-free
  // for TypeScript projects which do not install @types/node globally.
  // @ts-ignore
  const readline = await import("node:readline");
  const lines = readline.createInterface({ input, crlfDelay: Infinity });
  const active = new Map<string, AbortController>();
  const tasks = new Set<Promise<void>>();
  const semaphore = new Semaphore(maxConcurrency);
  let initialized = false;
  let shuttingDown = false;

  const write = (message: Record<string, unknown>) => {
    const line = JSON.stringify(message);
    if (byteLength(line) + 1 > maxFrameBytes) throw new Error(`response exceeds ${maxFrameBytes} bytes`);
    output.write(`${line}\n`);
  };
  const respond = (id: string, result: unknown) => write({ jsonrpc: "2.0", id, result });
  const respondError = (id: string, code: number, message: string) => write({ jsonrpc: "2.0", id, error: { code, message } });
  const cancelAll = () => { for (const controller of active.values()) controller.abort(); };

  const execute = (id: string, action: Action, tool: Tool): Promise<void> => {
    const controller = new AbortController();
    active.set(id, controller);
    const task = (async () => {
      let release = () => {};
      try {
        release = await semaphore.acquire(controller.signal);
        let result: ToolResult;
        try {
          result = await tool.handler(action, controller.signal);
        } catch (error) {
          result = { ok: false, error: errorMessage(error) };
        }
        if (result === null || typeof result !== "object" || Array.isArray(result)) {
          result = { ok: false, error: "handler returned a non-object ToolResult" };
        }
        // Handler output is untrusted: normalize both routing fields.
        result.action_id = action.id;
        result.kind = action.kind;
        respond(id, result);
      } catch (error) {
        respond(id, { ok: false, action_id: action.id, kind: action.kind, error: errorMessage(error) });
      } finally {
        release();
        active.delete(id);
      }
    })();
    tasks.add(task);
    task.then(() => tasks.delete(task), () => tasks.delete(task));
    return task;
  };

  try {
    for await (const line of lines as AsyncIterable<string>) {
      if (byteLength(line) > maxFrameBytes) throw new Error(`request exceeds ${maxFrameBytes} bytes`);
      if (!line.trim()) throw new Error("blank protocol frame");
      let request: any;
      try { request = JSON.parse(line); } catch (error) {
        respondError("", -32700, errorMessage(error));
        continue;
      }
      if (request?.jsonrpc !== "2.0" || typeof request.method !== "string") {
        respondError(typeof request?.id === "string" ? request.id : "", -32600, "invalid JSON-RPC request");
        continue;
      }
      if (request.method === "$/cancelRequest") {
        const id = request.params?.id;
        if (typeof id === "string") active.get(id)?.abort();
        continue;
      }
      if (typeof request.id !== "string" || !request.id) {
        respondError("", -32600, "request id must be a string");
        continue;
      }
      if (!initialized) {
        if (request.method !== "plugin/initialize") {
          respondError(request.id, -32600, "plugin must be initialized first");
          continue;
        }
        const params = request.params;
        const supported = params?.supported_capabilities?.[TOOL_PROVIDER] ?? [];
        if (params?.runtime_protocol !== RUNTIME_PROTOCOL || params?.host?.placement !== "hands" || !supported.includes(TOOL_PROVIDER_VERSION)) {
          respondError(request.id, -32602, "incompatible runtime or placement");
          continue;
        }
        const configuration = { max_concurrency: maxConcurrency, tools: server.tools.map(({ kind, description, input_schema }) => ({ kind, description, input_schema })) };
        respond(request.id, { plugin: { name: server.name, version: server.version ?? "0.1.0" }, capabilities: [{ type: TOOL_PROVIDER, version: TOOL_PROVIDER_VERSION, configuration }] });
        initialized = true;
        continue;
      }
      if (request.method === "plugin/health") {
        respond(request.id, { status: "ok" });
      } else if (request.method === "plugin/shutdown") {
        respond(request.id, {});
        shuttingDown = true;
        cancelAll();
        await Promise.race([Promise.all([...tasks]), new Promise<void>(resolve => setTimeout(resolve, shutdownTimeoutMs))]);
        return;
      } else if (request.method === "tools/execute") {
        const action = request.params?.action;
        if (!action || typeof action.kind !== "string" || !action.kind || action.args === null || typeof action.args !== "object" || Array.isArray(action.args)) {
          respondError(request.id, -32602, "action with object args is required");
          continue;
        }
        const tool = tools.get(action.kind);
        if (!tool) {
          respond(request.id, { action_id: action.id ?? "", kind: action.kind, ok: false, error: `no tool ${JSON.stringify(action.kind)}` });
          continue;
        }
        void execute(request.id, { id: String(action.id ?? ""), kind: action.kind, args: action.args }, tool);
      } else {
        respondError(request.id, -32601, "method not found");
      }
    }
  } finally {
    if (!shuttingDown) cancelAll();
    lines.close();
  }
}
