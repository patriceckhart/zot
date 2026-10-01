# Working Agreement for zot

This file defines how automated coding assistants should work in this repository. Treat it as a practical operating manual, not a substitute for reading the code.

## Product intent

zot is a compact Go coding-agent harness. Changes should preserve its defining properties:

- one portable binary
- a provider-neutral agent engine
- a terminal UI without a framework dependency
- extensions that communicate through subprocess JSON-RPC
- predictable behavior on macOS, Linux, and Windows
- a small dependency and operational footprint

Prefer a narrow, explicit implementation over a generalized subsystem. New abstractions must earn their cost through a real boundary or repeated use.

## Starting a task

Build context before editing:

1. Inspect `git status --short`. Existing modifications may belong to the user or another agent.
2. Locate and read every `AGENTS.md` that governs the target path.
3. Read the owning implementation, nearby tests, and any user-facing documentation for the behavior.
4. Reproduce reported failures when feasible. Record expected behavior separately from observed behavior.
5. For GitHub work, inspect the issue or pull request with `gh` if it is available. Do not change branches merely to review a pull request.

Do not use a search result or one function as a complete model of a feature. Follow calls across package boundaries and inspect persisted or streamed representations when relevant.

## Code ownership map

Put behavior in the package that owns the concern:

| Area | Responsibility |
|---|---|
| `packages/core` | Agent loop, messages, tool contracts, events, confirmation, sessions, compaction |
| `packages/provider` | Provider clients, wire formats, streaming, retries, model metadata and discovery |
| `packages/provider/auth` | Login flows, credential lookup, refresh, and storage |
| `packages/agent` | CLI configuration, model selection, runtime assembly, RPC, and .zot agents |
| `packages/agent/modes` | Interactive, print, JSON, dialog, bot, and Telegram interfaces |
| `packages/agent/extensions` | Extension process lifecycle and host integration |
| `packages/agent/extproto` | Extension wire protocol |
| `packages/agent/ext` | Public extension SDK |
| `packages/agent/skills` | Skill discovery and loading |
| `packages/agent/swarm` | Background-agent state, supervision, and persistence |
| `packages/agent/tools` | Built-in tools, permission checks, and jail behavior |
| `packages/tui` | Terminal input, layout, rendering, markdown, themes, and images |
| `packages/ignore` | Ignore-pattern matching |

Provider quirks must not leak into `packages/core`. Terminal escape handling and visual layout belong in `packages/tui`. Avoid making the interactive mode a catch-all when a focused package can own the behavior.

## Correctness contracts

Some parts of zot require extra care because small mistakes corrupt state or weaken user protections.

### Agent events and sessions

- Keep streamed events and stored transcripts structurally valid.
- Preserve tool-call and tool-result pairing, including cancellation and error paths.
- Maintain compatibility with existing session files unless a migration is explicitly part of the task.
- Validate replay, resume, fork, import, and export paths when changing shared session structures.

### Providers

- Keep request construction, response parsing, streaming state, and retry classification inside `packages/provider`.
- Never assume two OpenAI-compatible services have identical edge behavior.
- When changing model metadata, verify provider ID, model ID, context size, output limit, reasoning support, routing, and pricing independently.
- Tests must use local servers or fixtures. Do not make paid or credentialed API calls.

### Credentials and private data

- Treat API keys, OAuth tokens, session text, extension payloads, and local state as sensitive.
- Never include secrets in logs, fixtures, snapshots, errors, or commits.
- Credential files must retain restrictive permissions where the platform supports them.
- Test authentication with synthetic values and isolated temporary directories.

### Tools and confinement

- Permission checks must fail closed.
- Resolve path and symlink behavior before allowing filesystem access.
- Jail mode is an accident-prevention guardrail, not a security sandbox. Documentation and errors must not claim stronger isolation.
- Preserve confirmation semantics across every mode. A convenient fallback must not silently grant access.

### Extensions and RPC

- Existing JSON-RPC and extension clients are compatibility surfaces.
- Add fields in a backward-compatible way and tolerate peers that do not send newly introduced fields.
- Keep stdout machine-readable where it carries protocol data. Diagnostics belong on stderr or in the established event channel.
- Ensure child processes are cleaned up on normal exit, cancellation, and startup failure.

### Terminal behavior

- Rendering code must account for ANSI sequences, Unicode display width, wrapping, narrow terminals, and partial streamed content.
- Avoid tests that depend on a developer's terminal capabilities.
- Changes involving input, resize, clipboard, process control, paths, shells, or signals require inspection of build-tagged counterparts.

## Implementation approach

For fixes, locate the earliest broken invariant and test there. A UI symptom may originate in provider parsing, event construction, or session replay. Repair the owning layer rather than masking the symptom downstream.

For features, identify all public surfaces before coding: flags, configuration, slash commands, SDK, RPC, extensions, persisted state, provider behavior, and docs. Implement only the surfaces required for a complete first version.

While editing:

- Preserve unrelated working-tree changes.
- Use idiomatic Go and standard-library facilities when they are sufficient.
- Keep functions and interfaces focused. Do not introduce an interface solely to mock one call unless it improves the production boundary.
- Return errors with enough context to identify the failed operation, without exposing private data.
- Thread `context.Context` through blocking work and honor cancellation.
- Avoid time-based synchronization in tests. Prefer channels, explicit hooks, or polling with a deadline.
- Use `t.TempDir()` and `t.Setenv()` for isolated tests. Restore package globals in cleanup.
- Do not perform opportunistic refactors outside the requested change.

If intentional behavior appears removable or requirements conflict materially, ask the user before proceeding.

## Validation ladder

Match validation effort to the change, then finish with repository-wide checks when code changed.

1. Add or update a regression test in the owning package.
2. During iteration, run the smallest relevant test, for example:

   ```sh
   go test ./packages/provider -run TestName
   ```

3. Format every changed Go file with `gofmt`.
4. Run the complete suite:

   ```sh
   go test ./...
   ```

   `make test` additionally enables the race detector and is preferred for concurrency-sensitive work.

5. Run `go vet ./...` for changes involving concurrency, interfaces, formatting calls, unsafe behavior, process lifecycle, or broad runtime wiring.
6. Before reporting completion, inspect:

   ```sh
   git diff --check
   git status --short
   git diff
   ```

Do not hide failures, weaken assertions, or claim validation that was not run. If a check cannot run, state the command and the reason.

Documentation-only changes do not require the Go test suite unless they alter generated or executable examples. They still require diff and status review.

## Documentation duties

Update documentation in the same change whenever users, integrators, or extension authors would observe different behavior. Relevant locations include `README.md`, `docs/rpc.md`, `docs/extensions.md`, `docs/skills.md`, `docs/themes.md`, and `docs/zotfiles.md`.

Examples must match real flags and schemas. State limitations plainly, especially around sandboxing, provider support, compatibility, and platform differences.

## Dependency policy

zot's single-binary design and small footprint are product requirements.

- Prefer the standard library or existing dependencies.
- Explain why a new dependency is necessary before adding it.
- Review transitive dependencies and platform implications.
- Do not add a TUI framework for behavior the current renderer can support.
- Keep `go.mod` and `go.sum` changes limited to the requested work.

## Source-control safety

The working directory may contain changes from other people or agents.

Allowed inspection commands include `git status`, `git diff`, `git log`, `git show`, and `git blame`. Avoid destructive or broad commands such as `git reset --hard`, `git clean`, `git checkout .`, and blanket staging.

Do not commit, create or switch branches, rebase, push, tag, or open a pull request unless the user explicitly requests that operation. Permission to edit files is not permission to commit them.

When a commit is requested:

- include only files changed for the current task
- stage paths explicitly
- review the staged diff before committing
- use a concise imperative subject that describes the behavior
- never bypass hooks without explicit approval

When a pull request is requested, run the full validation suite first, summarize compatibility and user-visible effects, and mention any failed or skipped checks.

## Completion report

A final response should be brief and factual. Include:

- what changed and where
- which checks were run and their results
- any unresolved risk, skipped validation, or follow-up needed

Do not report success while tests are failing. Do not include unrelated cleanup suggestions unless they materially affect the requested work.
