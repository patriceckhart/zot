---
name: test-fix
description: Diagnose and minimally fix a failing test, then re-run.
allowed-tools: [read, edit, bash]
---

# Test fix

When the user reports a failing test (or a test suite failure), work
in this order. Resist the temptation to rewrite large chunks of code.

## 1. Reproduce

Run the failing test in isolation first. For Go: `go test -run
TestThing ./pkg/...`. For Node: `npm test -- --run TestThing`. The
goal is a fast feedback loop, not the whole suite.

## 2. Read the failure carefully

- What was expected vs got?
- Which line of test code triggered the failure?
- Is it a logic error in the implementation, an outdated test
  expectation, or environmental drift (e.g. timezone, locale, file
  system case-sensitivity)?

## 3. Choose the smallest possible fix

- If the test is wrong → update the assertion only. Don't restructure.
- If the implementation is wrong → patch the smallest function that
  fixes the failing case. Avoid drive-by refactors.
- If the test depends on behaviour you need to change deliberately →
  ask the user before changing the assertion.

## 4. Re-run

The same isolated test, then the immediate suite, then the full
test command if applicable. Stop the moment you see green; don't
keep poking.

## 5. Report

Write 2–3 sentences: what was wrong, what changed, and which command
verifies the fix. Include the diff inline.
