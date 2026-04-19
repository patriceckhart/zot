# zot skills

A skill is a reusable instruction set written as a single
`SKILL.md` file with a YAML frontmatter header. zot discovers skills
at startup and surfaces them to the model in two ways:

1. The system prompt gains a short manifest:
   `Available skills: ... - code-review — Run a self-review pass...`
2. A built-in `skill` tool lets the model load any one skill's full
   body on demand.

The on-demand-load model keeps token usage cheap: only the manifest
goes into every request; the body is fetched as a tool result the
one or two turns the model actually needs it.

## Anatomy

```markdown
---
name: code-review
description: Run a thorough self-review pass on a recent change.
allowed-tools: [read, bash]
permissions:
  bash: ["git diff*", "git log*"]
---

# Code review

When asked to review code, ...
```

### Frontmatter fields

| field | required | purpose |
|---|---|---|
| `name` | optional | skill identifier; defaults to the directory name |
| `description` | required | one-line summary shown in the system prompt |
| `allowed-tools` | optional | list of tool names the skill is meant to use; informational |
| `permissions` | optional | per-tool patterns; informational |

`allowed-tools` and `permissions` are **parsed but not enforced** in
this version. They appear in the rendered skill body so the model can
see them and self-regulate. Future versions may enforce.

The body (everything after the second `---`) is plain markdown.
There's no template engine; the model sees what you write.

## Discovery

zot looks in these directories, in priority order, and registers the
first `SKILL.md` it finds for each unique name:

| location | scope |
|---|---|
| `./.zot/skills/<name>/SKILL.md` | project (native) |
| `$ZOT_HOME/skills/<name>/SKILL.md` | global (native) |
| `./.claude/skills/<name>/SKILL.md` | project (claude-compat) |
| `~/.claude/skills/<name>/SKILL.md` | global (claude-compat) |
| `./.agents/skills/<name>/SKILL.md` | project (agent-compat) |
| `~/.agents/skills/<name>/SKILL.md` | global (agent-compat) |

The compat paths are deliberate: a `SKILL.md` written for an existing
skill ecosystem works in zot unchanged. Drop your existing
`.claude/skills/` or `.agents/skills/` directories into a project and
zot will pick them up.

`$ZOT_HOME` defaults to `~/Library/Application Support/zot/` on macOS,
`$XDG_STATE_HOME/zot` on Linux, `%LOCALAPPDATA%\zot` on Windows.

## Inspecting installed skills

In zot, run `/skills`. A picker lists every discovered skill with its
description and source path. Press enter on a row to view the full
body inline. Press esc to go back.

## How the model uses a skill

1. The system prompt tells the model that skills exist and what
   their names + descriptions are.
2. The model recognises a request that maps to a known skill and
   calls the `skill` tool with `name: "<skill-name>"`.
3. The `skill` tool returns the markdown body as the tool result.
4. The model follows the body's instructions.

You can prompt the model directly to use a skill (e.g. "use the
code-review skill") but you don't have to — the descriptions in the
manifest are enough for it to choose on its own.

## Writing good skills

- **Be procedural.** Number steps. Tell the model what to do in what
  order. Skills are habits, not knowledge dumps.
- **Be precise about boundaries.** "Stop after step 4" is more
  effective than "don't go too far".
- **Trim aggressively.** A 200-line skill bloats every turn the
  model uses it. Aim for 20–80 lines.
- **One skill per behaviour.** Don't pack three workflows into one
  SKILL.md; the model picks one path. Two separate skills work better.
- **Lead with the trigger.** First paragraph should make it
  obvious *when* to use the skill so the model self-selects correctly.

## Examples

See `examples/skills/` for two starter skills:

- `code-review/` — self-review pass on a recent diff
- `test-fix/` — diagnose + minimally fix a failing test

## Comparison to other discovery layouts

| ecosystem | path | zot reads it? |
|---|---|---|
| (native) | `.zot/skills/<name>/SKILL.md` | yes |
| (claude-style) | `.claude/skills/<name>/SKILL.md` | yes |
| (agent-style) | `.agents/skills/<name>/SKILL.md` | yes |

Cross-pollination is intentional: pick whichever convention you're
already using and zot tags along.
