---
name: code-review
description: Run a thorough self-review pass on the most recent change.
allowed-tools: [read, bash]
permissions:
  bash: ["git diff*", "git log*", "git show*", "git status"]
---

# Code review

When the user asks for a code review (and you have not already done
one in this turn), follow this routine.

## 1. Establish what changed

Use `bash` to run `git status` and `git diff` (or `git diff --staged`
if there are no unstaged changes). Skim the patch end to end before
analysing any single hunk.

## 2. For each modified file

Read the file in full with the `read` tool — never review only the
hunk; you need surrounding context to evaluate the change properly.
Then look for:

- **Correctness**: bugs, off-by-one errors, wrong sign, missing nil
  checks, swapped arguments, race conditions.
- **Error handling**: every external call (file IO, network, parsing,
  syscalls) — does it propagate or swallow? Are errors wrapped with
  enough context?
- **Tests**: do the new code paths have tests? Are existing tests
  still passing what they claim?
- **Surface area**: are exports necessary, or could the change stay
  internal? Public APIs deserve more scrutiny than internals.
- **Style consistency**: does the change match neighbouring code?

## 3. Report

Produce a concise written review with this shape:

- **Verdict** (one line): ship-as-is / minor changes / needs work / blocked.
- **Required changes** (numbered, if any).
- **Suggestions** (bullets, optional).
- **Praise** (one or two lines if anything stood out — keeps the
  feedback humane).

Don't restate every line of the diff. Don't speculate about future
features. Stay grounded in what the patch does.

## 4. Stop

Do not auto-apply fixes. The user will decide what to act on.
