# zotfile agents

A zotfile packages an agent's behavior into one portable `.zot` file. It can contain the agent's instructions, reusable skills, static assets, and metadata describing the runtime, model, operating-system, binary, and tool permissions it needs.

The current implementation supports creating, inspecting, verifying, and running local directories and `.zot` archives. It can also run an agent repository or directory directly from any public GitHub repository without keeping a clone. zot has no built-in owner or official collection. Indexed registry distribution, installation, signatures, bundled executable extensions, network permissions, and environment permissions are not implemented yet.

## Quick start

Create a directory for the agent:

```text
reviewer/
├── manifest.json
├── AGENT.md
└── skills/
    └── code-review/SKILL.md
```

Add a minimal `manifest.json`:

```json
{
  "zotfile": 1,
  "name": "code-reviewer",
  "version": "0.1.0",
  "description": "Reviews a repository and reports actionable findings.",
  "runtime": {
    "min_zot": "0.2.76"
  },
  "model": {
    "requires": ["tools"],
    "min_context": 64000
  },
  "permissions": {
    "fs": {
      "read": ["${workspace}"],
      "write": []
    },
    "bash": {
      "mode": "none"
    }
  },
  "requirements": {
    "os": ["darwin", "linux", "windows"]
  },
  "entry": {
    "greeting": "What should I review?",
    "default_prompt": null
  }
}
```

Add the agent's standing instructions to `AGENT.md`:

```markdown
# Code reviewer

Review the current repository without modifying it.

Prioritize correctness, security, regressions, and missing tests. Report findings
in severity order with file and line references. Do not praise the code or
summarize files that have no findings.
```

Test the directory directly during development:

```bash
zot inspect ./reviewer
zot run ./reviewer
zot run ./reviewer "Review the authentication package"
```

Package it when ready:

```bash
zot pack ./reviewer
# wrote code-reviewer.zot
# digest sha256:...

zot verify ./code-reviewer.zot
zot run ./code-reviewer.zot
```

The first run displays the declared permissions and asks for consent before the agent starts.

## Directory layout

```text
my-agent/
├── manifest.json          # identity, requirements, and permissions, required
├── AGENT.md               # persona and standing instructions
├── skills/                # SKILL.md directories, optional
│   └── my-skill/SKILL.md
├── assets/                # static files, optional
└── README.md              # human-facing documentation, optional
```

Only `manifest.json`. Other ordinary files and directories are included in the archive, but the runtime gives no special behavior to `assets/` or `README.md` yet. Files in `assets/` are not automatically added to the model context or granted filesystem access.

Do not include executable extensions in `extensions/`. The local runtime rejects any bundled extension directory containing `extension.json` because extension subprocesses cannot yet be confined to the manifest permissions. This is a deliberate fail-closed restriction.

Symlinks are not supported and cause `zot pack` to fail.

## `AGENT.md`

`AGENT.md` is optional. When present, it is appended to zot's system prompt by default. It should describe the agent's role, workflow, constraints, output format, and when to load bundled skills. When absent, the system prompt is left unchanged.

Keep capability and security declarations out of this file. Permissions come only from `manifest.json` and are enforced independently of agent-authored prose.

Set `replace_system_prompt` to `true` in the manifest to use `AGENT.md` as the replacement system prompt:

```json
{
  "replace_system_prompt": true
}
```

Replacement is intended for fully specialized agents. The default layering behavior is usually preferable because it retains zot's normal identity and tool-use guidance. With replacement enabled, an empty `AGENT.md` intentionally omits the built-in identity while retaining generated context.

## Bundled skills

Place skills under `skills/<name>/SKILL.md` using the normal zot skill format:

```text
skills/
└── investigate-failure/
    └── SKILL.md
```

```markdown
---
name: investigate-failure
description: Diagnose a failing command from its output and relevant source files.
---

# Investigate failure

1. Reproduce the failure if permissions allow it.
2. Identify the first meaningful error rather than downstream noise.
3. Read the implementation and nearest tests.
4. Propose the smallest correction and validation plan.
```

Bundled skills are added to normal skill discovery while the zotfile is running. The model sees their name and description in the skill manifest and can load the full body through the `skill` tool. See [skills.md](skills.md) for the complete skill format.

## Manifest reference

The current manifest shape is:

```json
{
  "zotfile": 1,
  "name": "code-reviewer",
  "version": "0.1.0",
  "description": "Reviews a repository and reports actionable findings.",
  "license": "MIT",
  "runtime": {
    "min_zot": "0.2.76"
  },
  "model": {
    "requires": ["tools", "reasoning"],
    "min_context": 64000,
    "preferred": ["claude-opus-4-5", "gpt-5.5"],
    "min_tier": ""
  },
  "permissions": {
    "fs": {
      "read": ["${workspace}", "${agent_data}"],
      "write": ["${agent_data}"]
    },
    "bash": {
      "mode": "allowlist",
      "allow": ["git", "go"]
    },
    "net": {
      "allow": []
    },
    "env": {
      "read": []
    }
  },
  "requirements": {
    "bin": ["git", "go"],
    "os": ["darwin", "linux"]
  },
  "entry": {
    "greeting": "What should I review?",
    "default_prompt": null
  },
  "replace_system_prompt": false
}
```

### Top-level fields

| Field | Required | Current behavior |
|---|---:|---|
| `zotfile` | yes | Format version. Must be `1`. |
| `name` | yes | Local agent identity and session/data namespace. See naming rules below. |
| `version` | no | Displayed in consent and inspection output. Semver is recommended but not currently validated. |
| `description` | no | Displayed by `zot inspect`. |
| `license` | no | Package metadata. Not otherwise interpreted by the local runtime. |
| `runtime.min_zot` | no | Minimum zot version. Older binaries refuse to run the agent. Unversioned development builds cannot satisfy a non-empty minimum. |
| `model` | no | Model capabilities, minimum context, and preferences. |
| `permissions` | no | Filesystem and bash permission ceiling. Omitted scopes deny access. |
| `requirements` | no | Required operating systems and executables. |
| `entry` | no | Initial presentation and prompt metadata. |
| `replace_system_prompt` | no | Replaces rather than appends to the normal system prompt when `true`. Defaults to `false`. |

### Name rules

The local runtime currently accepts a flat lowercase name containing only:

- `a` to `z`
- `0` to `9`
- `.`, `-`, and `_`

Examples:

```text
code-reviewer
repo_onboarder
acme.audit
```

Names containing `/`, uppercase letters, spaces, or path-like values such as `..` are rejected. Registry-style `namespace/agent` names are reserved for the future distribution runtime and are not accepted locally yet.

### Model requirements

`model.requires` currently recognizes:

| Capability | Behavior |
|---|---|
| `tools` | Accepted. Every model in the current catalog is treated as tool-capable. |
| `reasoning` | Requires a catalog model marked as supporting reasoning. |
| `vision` | Recognized, but currently fails closed because vision support is not represented in the model catalog. No model qualifies. |

Unknown capabilities are rejected. Use `tools`, not the draft spelling `tool_calling`.

`model.min_context` requires a catalog model with at least that many context tokens.

`model.preferred` is an ordered list of model IDs. It is a preference, not a provider requirement. Selection proceeds as follows:

1. An explicitly selected model, if present, must satisfy the requirements.
2. The user's configured default is retained if it satisfies them.
3. The first compatible model in `preferred` is selected.
4. Otherwise, zot chooses a compatible active catalog model.
5. If none qualifies, execution stops with an error.

`model.min_tier` is present in the format shape but is not supported by the local runtime. Leave it empty or omit it. A non-empty value is rejected.

### Runtime and host requirements

Use `requirements.os` to restrict the agent to Go operating-system names:

```json
{
  "requirements": {
    "os": ["darwin", "linux", "windows"]
  }
}
```

Use `requirements.bin` for commands that must already be on `PATH`:

```json
{
  "requirements": {
    "bin": ["git", "python3"]
  }
}
```

Zot checks these requirements before requesting consent. These zotfiles do not have install or postinstall hooks, so authors must document how users can obtain missing programs.

### Entry fields

`entry.pre` is auto-submitted once when `zot run` starts, before the initial prompt is applied. Values that begin with `!` use the interactive shell-escape path (or `BashTool` in `--print` / `--stream` / `--json`). Shell output streams live to the TUI (interactive) or stderr (non-interactive). Other values are sent as a normal user turn; in `--stream` mode that turn's assistant text also streams to stdout. After `pre` finishes, zot reloads extensions and rediscovers skills so anything installed by that command is available for the following turn.

```json
{
  "entry": {
    "pre": "!npx -y skills add tt-a1i/archify --skill archify --agent cursor --copy --yes"
  }
}
```

`entry.default_prompt` is used when the user runs the agent without a prompt. It may be a string or `null`:

```json
{
  "entry": {
    "default_prompt": "Review the current repository"
  }
}
```

In interactive mode, when no prompt is given on the command line, `default_prompt` pre-fills the editor after `pre` finishes (or immediately when `pre` is empty). It is not auto-submitted. A prompt supplied on the command line takes precedence the same way.

In `--print` / `--stream` / `--json` modes, `pre` still runs first: shell escapes abort the run on failure; plain-text `pre` is sent as a first agent turn before the main prompt (`default_prompt` or the CLI prompt).

`entry.greeting` is part of the manifest, but the current local runtime does not render it yet.

## Filesystem permissions

Filesystem permissions are enforced by the built-in `read`, `write`, and `edit` tools. Empty or omitted scopes deny the operation.

Two variables are available in filesystem scopes:

| Variable | Resolves to |
|---|---|
| `${workspace}` | The current working directory used for the run. |
| `${agent_data}` | The agent's persistent private data directory under `$ZOT_HOME/agents/<name>/data/`. |

Relative paths are resolved beneath `${workspace}`. For example:

```json
{
  "permissions": {
    "fs": {
      "read": ["src", "go.mod"],
      "write": ["${agent_data}"]
    }
  }
}
```

This allows reads under `<workspace>/src` and at `<workspace>/go.mod`, plus writes under the private agent data directory. It does not grant general workspace writes.

Common permission profiles follow.

Read-only repository agent:

```json
{
  "permissions": {
    "fs": {
      "read": ["${workspace}"],
      "write": []
    },
    "bash": { "mode": "none" }
  }
}
```

Repository editor with private persistent state:

```json
{
  "permissions": {
    "fs": {
      "read": ["${workspace}", "${agent_data}"],
      "write": ["${workspace}", "${agent_data}"]
    },
    "bash": { "mode": "none" }
  }
}
```

Data-only agent:

```json
{
  "permissions": {
    "fs": {
      "read": ["${agent_data}"],
      "write": ["${agent_data}"]
    },
    "bash": { "mode": "none" }
  }
}
```

Filesystem checks canonicalize existing paths, or the nearest existing parent for a new path, so symlinks cannot be used to escape a declared scope.

If the working directory changes during a run, `${workspace}` permissions are expanded again for the new directory.

## Bash permissions

`permissions.bash.mode` supports three values.

### `none`

```json
{
  "bash": { "mode": "none" }
}
```

All bash tool calls are denied. This is also the default when `mode` is omitted.

### `ask`

```json
{
  "bash": { "mode": "ask" }
}
```

Bash is available after the user approves the agent at launch. Consent for an agent using `ask` is requested on every launch and is not cached.

In the current local runtime, `ask` is launch-time capability consent. It does not open a separate confirmation dialog for every individual bash invocation.

### `allowlist`

```json
{
  "bash": {
    "mode": "allowlist",
    "allow": ["git", "go", "grep"]
  }
}
```

Every command in a shell expression must be listed. Zot checks commands separated by `;`, `&&`, `||`, and pipes. Paths are reduced to their base command name, so `/usr/bin/git` is checked as `git`. A leading environment assignment is skipped when identifying the command.

The allowlist rejects shell substitution, newlines, redirection, backticks, `$()`, `<`, and `>`. This intentionally supports simple command pipelines rather than arbitrary shell programs.

An `allowlist` mode with an empty `allow` array is invalid.

Only declare commands the agent genuinely needs. `requirements.bin` checks that a command exists, while `permissions.bash.allow` controls whether the agent may invoke it. Agents that execute a required command normally need it in both places.

## Network and environment permissions

The manifest shape includes `permissions.net.allow` and `permissions.env.read`, but the local runtime does not enforce them yet. To avoid presenting unenforced declarations as security controls, it rejects manifests containing any network host or environment variable.

Use empty arrays or omit these sections:

```json
{
  "permissions": {
    "net": { "allow": [] },
    "env": { "read": [] }
  }
}
```

Bundled executable extensions are rejected for the same reason. Network filtering, scrubbed extension environments, and extension confinement belong to a later runtime milestone.

## Consent and persistent data

Before the first run, zot prints the agent identity and expanded filesystem and bash permissions, then asks:

```text
Agent code-reviewer@0.1.0 wants to run.

  fs read: /path/to/workspace
  fs write: none
  bash: none

Allow? [y/N]
```

For modes other than `bash: ask`, approval is cached for the exact artifact digest under:

```text
$ZOT_HOME/agents/<name>/consents/<digest>.json
```

Any change to the packaged artifact produces a different digest and requires consent again. `bash: ask` always requires fresh launch consent.

Non-interactive runs refuse to bypass consent by default. For controlled automation, pass `-y` / `--yes` or set:

```bash
zot run -y ./code-reviewer.zot --print "Review this repository"
```

```bash
ZOT_AGENT_CONSENT=1 zot run ./code-reviewer.zot --print "Review this repository"
```

This environment variable skips the consent prompt. Only use it after independently inspecting and trusting the exact artifact being run.

Persistent agent data lives under:

```text
$ZOT_HOME/agents/<name>/data/
```

The directory is created for every run, but the agent can access it only when `${agent_data}` appears in the relevant filesystem permission scope.

## Agent-scoped sessions

Sessions created by a zotfile are isolated from ordinary zot sessions and from other agents:

```text
$ZOT_HOME/sessions/agents/<name>/
```

Normal session flags still apply, including `--continue`, `--resume`, `--session`, and `--no-session`, but their default storage root is scoped to the active agent. The interactive `/sessions` picker and `/session` import, fork, and tree commands use that same agent-scoped root.

## Commands

### `zot run`

Run an unpackaged directory during development:

```bash
zot run ./my-agent
zot run -y ./my-agent
zot run ./my-agent "Do the task"
zot run ./my-agent -y --print "Do the task"
```

Run a packed artifact:

```bash
zot run ./my-agent.zot
zot run ./my-agent.zot --print "Do the task"
zot run ./my-agent.zot --stream "Do the task"
zot run ./my-agent.zot --json "Do the task"
```

Arguments after the reference use zot's normal CLI parser, so model, provider, reasoning, cwd, session, tool, and output-mode flags remain available. The manifest still imposes its permission and compatibility ceiling. In interactive mode, enabling **show loaded resources at startup** in `/settings` displays the manifest name in an `[Agent]` section above the transcript.

Local filesystem directories, local archive paths, GitHub shorthand, and public GitHub agent-directory URLs are accepted. A single-part name resolves only to a matching local directory or `.zot` archive:

```bash
zot run my-agent
```

A two-part name is treated as a GitHub `owner/repository` reference. Resolution remains local-first, then uses the repository root:

```bash
zot run frkr/zot-archify
```

Use three or more parts to select an agent directory from any public GitHub collection:

```bash
zot run acme/agents/reviewer
zot run acme/clankers/reviewers/go
```

There is no built-in owner, official collection, collection configuration, or allowlist. Every shorthand part must use lowercase letters, digits, dots, hyphens, or underscores. Backslashes are also accepted as separators on Windows. Prefix a reference with `./`, use an absolute path, retain the `.zot` suffix, or provide a complete GitHub URL to force local or explicit remote resolution.

For GitHub, zot downloads the repository archive into a temporary directory, selects the requested agent subdirectory, validates it, runs it, and removes the downloaded files when the command exits:

```bash
zot run https://github.com/acme/agents/reviewer
```

A standard GitHub tree URL is also supported:

```bash
zot run https://github.com/acme/agents/tree/main/reviewer
```

Short forms read the repository's default branch through GitHub's `HEAD` archive. A tree URL uses the branch or tag in the URL. Private repositories and branch or tag names containing `/` are not currently supported. The downloaded source is temporary, but normal agent data, consent receipts, and session transcripts remain under `$ZOT_HOME`.

`owner/repository` and `owner/repository/path` are direct GitHub mappings, not an indexed or signed registry. Installed names, arbitrary URLs, and OCI references are not resolved yet.

### `zot pack`

```bash
zot pack [directory] [output]
```

Examples:

```bash
zot pack
zot pack ./my-agent
zot pack ./my-agent ./dist/reviewer.zot
```

The default directory is the current directory. When output is omitted, zot uses `<manifest-name>.zot`. If the output has no `.zot` suffix, zot adds one.

Packing validates the manifest and directory, then creates a zstd-compressed tar archive. Entries are sorted and tar metadata is normalized with fixed timestamps and numeric ownership. The output archive itself is excluded when it is located inside the source directory. Zot prints the SHA-256 digest of the resulting compressed artifact.

### `zot inspect`

```bash
zot inspect ./my-agent
zot inspect ./my-agent.zot
zot inspect https://github.com/acme/agents/code-reviewer
```

Inspection validates and prints the agent's name, version, description, digest, declared permissions, and complete file list. It does not execute the agent.

For a directory, the digest is computed from its canonical uncompressed tar representation. For a `.zot` archive, the digest is computed from the archive bytes. Therefore a source-directory digest and its packed-archive digest are not expected to match.

### `zot verify`

```bash
zot verify ./my-agent.zot
```

The local `verify` command validates that the archive can be safely loaded, validates the manifest and required layout, and prints its SHA-256 digest.

Despite the command name, it does **not** verify a publisher signature in the current runtime. Signature and namespace verification are planned for registry distribution.

## Packaging and extraction limits

The archive loader applies these limits:

| Limit | Value |
|---|---:|
| Compressed `.zot` file | 100 MiB |
| One extracted entry | 64 MiB |
| Total extracted content | 256 MiB |

Archive extraction rejects absolute paths and parent traversal. Unsupported tar entry types are ignored. New packages use zstd; gzip-compressed tar archives are accepted only as a compatibility fallback for earlier experiments.

## Authoring guidance

1. Start with the narrowest permissions possible. A reviewer usually needs repository reads, not writes or bash.
2. Keep `AGENT.md` focused on stable behavior. Put task-specific procedures in skills so the model loads them only when relevant.
3. Declare every external command in both `requirements.bin` and the bash allowlist when the agent must execute it.
4. Test from a directory first with `zot inspect` and `zot run`.
5. Test denied behavior as well as allowed behavior. Ask the agent to attempt an out-of-scope read, write, and command.
6. Pack the same source twice and compare behavior and output digests before distributing it.
7. Inspect and verify the final archive, not only its source directory.
8. Do not claim that network, environment, signatures, extensions, installation, or registry delivery are secured or supported by the local runtime.

## Current scope

Implemented now:

- local directories and `.zot` archives
- local-only resolution for single-part names
- local-first `owner/repository` and `owner/repository/path` resolution to any public GitHub repository
- temporary execution of agent directories from public GitHub repositories
- `zot pack`, `zot inspect`, `zot verify`, and `zot run`
- canonical tar creation with zstd compression
- archive digest reporting and safe extraction limits
- `AGENT.md` append or replacement behavior
- bundled skills
- runtime, OS, and binary requirements
- model context and reasoning requirements
- filesystem scope enforcement
- bash `none`, `ask`, and `allowlist` modes
- digest-scoped consent receipts
- private agent data directories
- agent-scoped sessions

Not implemented yet:

- `zot install`, `zot agents`, `zot use`, `zot update`, or `zot publish`
- installed-name, arbitrary URL, OCI, configurable registry, or zot.sh index resolution
- network allowlist enforcement
- environment-variable filtering
- safely confined bundled executable extensions
- automatic artifact theme loading
- agent dependency composition
- per-agent Telegram, bot, or RPC selection
- `--no-sandbox`, `--trust-bash`, or registry trust override flags

Treat the unsupported fields and commands in the broader zotfile proposal as forward-looking design, not as current runtime behavior.
