// clock — a zot extension written in plain Node (no dependencies).
//
// Registers two slash commands:
//   /now            — pushes the current local time into the chat as
//                     a one-shot note (no model call, no transcript)
//   /uptime         — submits a prompt asking the agent to comment on
//                     how long this extension has been running
//
// Why .js and not .ts: this file uses JSDoc types so it can be
// type-checked by tsc / tsserver without a build step. Renaming to
// .ts and updating extension.json's args to ["--import","tsx",
// "index.ts"] (with tsx installed) works too. The extension protocol
// itself is language-agnostic; what matters is that `exec` produces
// a process that reads JSON lines from stdin and writes them to
// stdout.
//
// Install:
//   zot ext install /path/to/this/dir
//
// Then in zot:
//   /now
//   /uptime

import { createInterface } from "node:readline";
import { stdin, stdout, stderr } from "node:process";

const NAME = "clock";
const VERSION = "1.0.0";
const STARTED_AT = Date.now();

/** @typedef {{type: string, id?: string, [k: string]: unknown}} Frame */

/**
 * Send a frame to zot. One JSON object per line; flush immediately
 * so the host doesn't sit waiting on a buffer.
 * @param {Frame} obj
 */
function send(obj) {
  stdout.write(JSON.stringify(obj) + "\n");
}

/**
 * stderr is captured by zot to $ZOT_HOME/logs/ext-clock.log; perfect
 * for debug output. Anything written to stdout would corrupt the
 * protocol stream.
 * @param {string} msg
 */
function log(msg) {
  stderr.write(`[${NAME}] ${msg}\n`);
}

// 1. Hello first.
send({
  type: "hello",
  name: NAME,
  version: VERSION,
  capabilities: ["commands"],
});

// 2. Register every command we can handle.
send({
  type: "register_command",
  name: "now",
  description: "show the current local time (no model call)",
});
send({
  type: "register_command",
  name: "uptime",
  description: "ask the agent to riff on how long the clock ext has run",
});

// 3. Read frames until stdin closes (zot shuts us down).
const rl = createInterface({ input: stdin, crlfDelay: Infinity });

rl.on("line", (line) => {
  /** @type {Frame} */
  let frame;
  try {
    frame = JSON.parse(line);
  } catch (err) {
    log(`malformed frame: ${err}`);
    return;
  }

  switch (frame.type) {
    case "hello_ack":
      log(
        `connected to zot ${frame.zot_version} (${frame.provider}/${frame.model})`,
      );
      break;

    case "command_invoked":
      handleCommand(frame);
      break;

    case "shutdown":
      send({ type: "shutdown_ack" });
      rl.close();
      break;

    default:
      log(`unknown frame type: ${frame.type}`);
  }
});

rl.on("close", () => {
  log("read loop closed; exiting");
  process.exit(0);
});

/**
 * @param {Frame & {name?: string, args?: string}} frame
 */
function handleCommand(frame) {
  const name = String(frame.name ?? "");
  const args = String(frame.args ?? "").trim();
  const id = String(frame.id ?? "");

  switch (name) {
    case "now": {
      const now = new Date();
      const human = now.toLocaleString(undefined, {
        weekday: "short",
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      });
      const iso = now.toISOString();
      send({
        type: "command_response",
        id,
        action: "display",
        display: `local: ${human}\niso  : ${iso}`,
      });
      return;
    }

    case "uptime": {
      const ms = Date.now() - STARTED_AT;
      const seconds = Math.round(ms / 1000);
      const focus = args ? `Focus on the topic: ${args}.` : "";
      send({
        type: "command_response",
        id,
        action: "prompt",
        prompt:
          `The clock extension has been running for ${seconds}s in this zot session. ` +
          `Riff on that briefly in one short sentence — be a little dramatic. ${focus}`.trim(),
      });
      return;
    }

    default:
      send({
        type: "command_response",
        id,
        action: "noop",
        error: `clock: unknown command /${name}`,
      });
  }
}
