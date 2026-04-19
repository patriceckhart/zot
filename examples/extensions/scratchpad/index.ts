// scratchpad — a real .ts zot extension with no SDK and no build step.
//
// Runs via `npx --yes tsx index.ts` (declared in extension.json).
// First invocation downloads tsx into npm's cache; every subsequent
// run is instant. Pure node + tsx, no other dependencies.
//
// What it does:
//
//   /note <text>    — append <text> to a process-local scratchpad
//   /notes          — show the scratchpad inline (no model call)
//   /clear-notes    — wipe the scratchpad
//
//   tool: read_notes() — the model can read the scratchpad on demand
//
// Use this as a template for any TypeScript extension that needs to
// register slash commands, expose tools to the model, or both.

import { createInterface } from "node:readline";
import { stderr, stdin, stdout } from "node:process";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";

// ---- protocol types (a tiny subset of internal/extproto) ----

type Frame = { type: string; id?: string; [k: string]: unknown };

interface CommandInvoked {
  type: "command_invoked";
  id: string;
  name: string;
  args?: string;
}

interface ToolCall {
  type: "tool_call";
  id: string;
  name: string;
  args: Record<string, unknown>;
}

interface HelloAck {
  type: "hello_ack";
  protocol_version: number;
  zot_version: string;
  provider: string;
  model: string;
  cwd: string;
}

type Action = "prompt" | "insert" | "display" | "noop";

interface CommandResponse {
  type: "command_response";
  id: string;
  action: Action;
  prompt?: string;
  insert?: string;
  display?: string;
  error?: string;
}

interface ToolContent {
  type: "text" | "image";
  text?: string;
  mime_type?: string;
  data?: string; // base64
}

interface ToolResult {
  type: "tool_result";
  id: string;
  content: ToolContent[];
  is_error?: boolean;
}

// ---- I/O helpers ----

const NAME = "scratchpad";
const VERSION = "1.0.0";

function send(frame: Frame): void {
  stdout.write(JSON.stringify(frame) + "\n");
}

function log(msg: string): void {
  // stderr is captured by zot to $ZOT_HOME/logs/ext-<name>.log;
  // safe for debug output. stdout is reserved for the protocol.
  stderr.write(`[${NAME}] ${msg}\n`);
}

// ---- the scratchpad state itself ----
//
// Notes persist as JSONL under <cwd>/.zot/scratchpad-notes.jsonl so
// they survive zot restarts and stay scoped to the project. The path
// is resolved once HelloAck arrives (which carries cwd); until then
// notesPath is empty and reads/writes no-op safely.
//
// One note per line, format:  {"at":"<iso>","text":"<body>"}
// Append-only on /note; full rewrite on /clear-notes.
//
// Single-writer assumption: only one zot session per cwd at a time.
// Concurrent writes from two zot instances would interleave but not
// corrupt JSONL line boundaries on POSIX (writes ≤ PIPE_BUF are
// atomic). Good enough for a demo.

type Note = { at: string; text: string };

let notes: Note[] = [];
let notesPath = "";

function setNotesPath(cwd: string): void {
  notesPath = join(cwd, ".zot", "scratchpad-notes.jsonl");
  loadNotes();
}

function loadNotes(): void {
  notes = [];
  if (!notesPath || !existsSync(notesPath)) return;
  try {
    const raw = readFileSync(notesPath, "utf8");
    for (const line of raw.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      try {
        const parsed = JSON.parse(trimmed) as Note;
        if (typeof parsed?.text === "string") notes.push(parsed);
      } catch {
        // skip malformed lines silently; the next /note will
        // append correctly anyway.
      }
    }
    log(`loaded ${notes.length} note(s) from ${notesPath}`);
  } catch (err) {
    log(`failed to read ${notesPath}: ${err}`);
  }
}

function appendNote(text: string): number {
  const note: Note = { at: new Date().toISOString(), text };
  notes.push(note);
  if (notesPath) {
    try {
      mkdirSync(dirname(notesPath), { recursive: true });
      appendFileSync(notesPath, JSON.stringify(note) + "\n", "utf8");
    } catch (err) {
      log(`failed to persist note to ${notesPath}: ${err}`);
    }
  }
  return notes.length;
}

function clearNotes(): void {
  notes = [];
  if (notesPath) {
    try {
      writeFileSync(notesPath, "", "utf8");
    } catch (err) {
      log(`failed to clear ${notesPath}: ${err}`);
    }
  }
}

function renderNotes(): string {
  if (notes.length === 0) return "(scratchpad is empty)";
  return notes
    .map((n, i) => `${i + 1}. [${n.at}] ${n.text}`)
    .join("\n");
}

// ---- handshake + registration ----

send({
  type: "hello",
  name: NAME,
  version: VERSION,
  capabilities: ["commands", "tools"],
});

send({
  type: "register_command",
  name: "note",
  description: "append text to the scratchpad",
});
send({
  type: "register_command",
  name: "notes",
  description: "show the scratchpad",
});
send({
  type: "register_command",
  name: "clear-notes",
  description: "wipe the scratchpad",
});

send({
  type: "register_tool",
  name: "read_notes",
  description:
    "Read the scratchpad. Use this when the user asks about notes or context they have stored, or refers to something from earlier they wanted you to remember.",
  schema: {
    type: "object",
    properties: {},
  },
});

// Sentinel: tells zot all initial registrations are flushed so the
// agent's tool registry can be built without racing the read loop.
send({ type: "ready" });

// ---- frame loop ----

const rl = createInterface({ input: stdin, crlfDelay: Infinity });

rl.on("line", (line: string) => {
  let frame: Frame;
  try {
    frame = JSON.parse(line) as Frame;
  } catch (err) {
    log(`malformed frame: ${err}`);
    return;
  }

  switch (frame.type) {
    case "hello_ack":
      handleHelloAck(frame as unknown as HelloAck);
      break;
    case "command_invoked":
      handleCommand(frame as unknown as CommandInvoked);
      break;
    case "tool_call":
      handleToolCall(frame as unknown as ToolCall);
      break;
    case "shutdown":
      send({ type: "shutdown_ack" });
      rl.close();
      break;
    default:
      log(`unknown frame: ${frame.type}`);
  }
});

rl.on("close", () => {
  log("read loop closed; exiting");
  process.exit(0);
});

function handleHelloAck(ack: HelloAck): void {
  log(
    `connected to zot ${ack.zot_version} ` +
      `(${ack.provider}/${ack.model}, cwd=${ack.cwd})`,
  );
  if (ack.cwd) setNotesPath(ack.cwd);
}

function handleCommand(frame: CommandInvoked): void {
  const args = (frame.args ?? "").trim();

  switch (frame.name) {
    case "note": {
      if (args === "") {
        respond(frame.id, {
          type: "command_response",
          id: frame.id,
          action: "noop",
          error: "note: usage is /note <text>",
        });
        return;
      }
      const n = appendNote(args);
      respond(frame.id, {
        type: "command_response",
        id: frame.id,
        action: "display",
        display: `noted (#${n}): ${args}`,
      });
      return;
    }

    case "notes": {
      respond(frame.id, {
        type: "command_response",
        id: frame.id,
        action: "display",
        display: renderNotes(),
      });
      return;
    }

    case "clear-notes": {
      clearNotes();
      respond(frame.id, {
        type: "command_response",
        id: frame.id,
        action: "display",
        display: "scratchpad cleared",
      });
      return;
    }

    default:
      respond(frame.id, {
        type: "command_response",
        id: frame.id,
        action: "noop",
        error: `unknown command /${frame.name}`,
      });
  }
}

function handleToolCall(frame: ToolCall): void {
  switch (frame.name) {
    case "read_notes": {
      sendToolResult(frame.id, [
        { type: "text", text: renderNotes() },
      ]);
      return;
    }
    default:
      sendToolResult(
        frame.id,
        [{ type: "text", text: `unknown tool ${frame.name}` }],
        true,
      );
  }
}

// ---- send wrappers (typed so misuse is a compile error) ----

function respond(_id: string, response: CommandResponse): void {
  send(response);
}

function sendToolResult(
  id: string,
  content: ToolContent[],
  is_error = false,
): void {
  const result: ToolResult = {
    type: "tool_result",
    id,
    content,
    is_error: is_error || undefined,
  };
  send(result);
}
