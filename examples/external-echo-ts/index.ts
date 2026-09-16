import { serve, type Action, type ToolResult } from "../../plugins/external/sdk/typescript/index.js";

const echoSchema = {
  type: "object",
  properties: {
    text: { type: "string", description: "Text to echo" },
    loud: { type: "boolean", description: "Upper-case the text" },
    repeat: { type: "integer", description: "Number of copies" }
  },
  required: ["text"],
  additionalProperties: false
};

type EchoArgs = { text?: unknown; loud?: unknown; repeat?: unknown };

const echo = async (action: Action, signal: AbortSignal): Promise<ToolResult> => {
  if (signal.aborted) return { ok: false, error: "canceled" };
  const args = action.args as EchoArgs;
  if (typeof args.text !== "string") return { ok: false, error: "text is required" };
  const repeat = typeof args.repeat === "number" && args.repeat > 0 ? Math.floor(args.repeat) : 1;
  const text = args.loud === true ? args.text.toUpperCase() : args.text;
  return { ok: true, output: text.repeat(repeat) };
};

await serve({
  name: "example.echo.ts",
  version: "1.0.0",
  tools: [{
    kind: "echo_text_ts",
    description: "Echo typed text from a TypeScript hands plugin.",
    input_schema: echoSchema,
    handler: echo
  }]
});
