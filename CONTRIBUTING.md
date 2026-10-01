# Contributing to zot

Contributions are welcome, including bug reports, design feedback, documentation, tests, and code. This guide explains what makes a contribution easy to evaluate and safe to merge.

## Choose the right starting point

### Reporting a defect

Use the bug-report template and provide enough information for someone else to reproduce the problem:

- the output of `zot --version`
- operating system and terminal
- provider and model, when relevant
- the smallest sequence of actions that triggers the problem
- expected and actual results
- relevant logs with credentials and private session content removed

Search existing issues first. If the behavior only occurs with a particular provider, include the HTTP status and sanitized response details when available, but never post an API key, OAuth token, authorization header, or full private transcript.

### Suggesting a capability

Start with the feature-request template. Describe the workflow that is difficult today before proposing an interface. Useful proposals explain:

- who encounters the problem
- why existing flags, extensions, or configuration do not solve it
- how the new behavior would appear in the CLI, TUI, SDK, or protocol
- compatibility or migration concerns
- reasonable alternatives

For changes that introduce a public API, alter stored data, modify an extension protocol, or span several packages, wait for design feedback before investing in an implementation.

### Small corrections

Focused typo, documentation, and test improvements can usually go directly to a pull request. Keep them separate from behavioral changes.

## Design boundaries

zot is intended to remain a lightweight, single-binary application. A contribution should fit that direction rather than increasing scope by default.

The major ownership boundaries are:

- `packages/core`: provider-independent agent behavior and session structures
- `packages/provider`: provider requests, streaming responses, model discovery, retries, and wire-specific behavior
- `packages/provider/auth`: credential and login handling
- `packages/agent`: command-line orchestration, configuration, runtime assembly, RPC, and .zot agents
- `packages/agent/modes`: interactive and non-interactive user modes
- `packages/agent/extensions`, `extproto`, and `ext`: extension hosting, protocol, and SDK
- `packages/agent/skills`: reusable instruction discovery
- `packages/agent/swarm`: background-agent supervision and state
- `packages/agent/tools`: built-in tools, permissions, and jail checks
- `packages/tui`: terminal mechanics and rendering
- `packages/ignore`: ignore matching

Keep provider-specific decisions out of the core agent loop. Keep terminal rendering out of orchestration code. If a feature can be delivered cleanly as an extension, consider that before expanding the built-in surface.

New dependencies need a clear benefit. The standard library and current dependency set are preferred, particularly for functionality that would otherwise increase binary size or complicate cross-platform builds.

## Preparing a development checkout

Use the Go version declared in `go.mod`, then build from the repository root:

```sh
go mod download
make build
./bin/zot --version
```

Useful commands:

```sh
make build       # compile ./bin/zot
make run         # build and start the interactive application
make test        # run all tests with the race detector
make lint        # run go vet and verify gofmt
```

Tests must not depend on real provider credentials, paid model calls, a developer's global zot state, or a specific terminal. Use local HTTP test servers, fixtures, temporary directories, and synthetic credentials.

## Making a change

A reviewable change has one purpose. Avoid bundling renames, formatting churn, generated metadata updates, or unrelated refactors into the same pull request.

For a bug fix:

1. Demonstrate the failure with a focused test when practical.
2. Find the package where the invalid state first appears.
3. Fix that layer rather than compensating in the UI or another downstream consumer.
4. Exercise neighboring cases that use the same parser, event path, or persisted format.

For new behavior:

1. Identify every user-visible and integration surface involved.
2. Preserve existing configuration, sessions, RPC clients, and extensions unless a breaking change has been agreed upon.
3. Add tests in the package that owns the behavior.
4. Update the relevant documentation in the same pull request.

Follow ordinary Go conventions. Format changed Go files with `gofmt`, propagate cancellation through `context.Context`, and wrap errors with useful operational context. Error messages and logs must not expose credentials or private content.

The repository's `AGENTS.md` contains additional implementation guidance for automated tools and is also useful as an architecture reference for human contributors.

## Areas requiring additional checks

### Providers and models

Provider contributions should cover request serialization, successful parsing, streamed events, API errors, and cancellation using local fixtures or an `httptest` server. Confirm model identifiers, context and output limits, reasoning support, provider routing, and pricing data from authoritative sources.

Do not treat all OpenAI-compatible endpoints as interchangeable. Service-specific behavior belongs in `packages/provider` and should be represented by tests.

### Sessions and event streams

Changes to messages or events must preserve valid tool-call and tool-result sequences. Check persistence and replay, not only live output. Consider resume, compaction, import, export, and forks whenever a shared session representation changes.

### RPC and extensions

The RPC and extension schemas are consumed outside this repository. Prefer additive changes, define behavior when a peer omits a new field, and keep protocol stdout free of human diagnostics. Update `docs/rpc.md` or `docs/extensions.md` for contract changes.

### Permissions and jail behavior

Permission failures must deny the operation. Test path traversal, symlink resolution, and cancellation where applicable. Jail mode is a safety guardrail rather than a hard isolation boundary, and contributions must preserve that distinction.

### Operating systems and terminals

CI runs on Linux, macOS, and Windows. Process handling, signals, shell invocation, filesystem paths, clipboard access, resize behavior, and terminal input often have platform-specific implementations. Inspect all build-tagged variants and avoid assumptions about Unix paths or ANSI support.

## Verification

Run focused package tests while developing. Before opening a pull request that changes Go code, run:

```sh
gofmt -w <changed-go-files>
go vet ./...
go test -race ./...
git diff --check
```

`make lint` and `make test` provide the repository equivalents for the lint and test steps.

Documentation-only pull requests do not need the Go suite unless they modify executable examples or generated content. They should still be checked for accurate commands, links, and formatting.

If a validation command cannot pass because of an unrelated repository or environment problem, describe the exact command and failure in the pull request. Do not silently omit it.

## Pull request shape

Open the pull request against `main` and include:

- the problem being addressed
- the chosen solution and why it belongs in zot
- tests added or updated
- commands used for verification
- user-visible, compatibility, and platform effects
- a linked issue when one exists

Keep commits understandable and use concise, imperative subjects. Update your branch normally when review requests changes. Do not force-push over review history unless there is a specific reason and reviewers have been informed.

A reviewer may ask for a contribution to be reduced in scope, moved to a different package, implemented as an extension, or split into independent pull requests. This is intended to keep behavior maintainable and does not diminish the underlying use case.

## Documentation locations

Choose the document that owns the public surface:

- `README.md` for installation, authentication, flags, common commands, modes, and user workflows
- `docs/providers.md` for provider-specific setup and behavior
- `docs/rpc.md` for the embedding protocol
- `docs/extensions.md` for extension authors
- `docs/skills.md` for reusable skills
- `docs/themes.md` for theme files
- `docs/zotfiles.md` for portable agents

Examples should be runnable and limitations should be stated directly.

## Sensitive security reports

Do not open a public issue containing an exploitable vulnerability, live credential, or private user data. Use GitHub's private vulnerability reporting for this repository when available. Include a minimal reproduction, affected versions, impact, and any suggested mitigation without using production secrets.

## License

By submitting a contribution, you agree that it may be distributed under the repository's [MIT License](LICENSE).
