# C# upstream feature sync: v1.14.44–v1.14.46

Reference: `CangShui/workbuddy-gateway` main
`e060d955c090460311a4c153161fdd2d0b0521ac`.
This ports behavior into the existing C# implementation. It does not merge the
upstream Go tree or remove the fork's existing billing, scheduling, or affinity code.

## Changes

- `7cee9d1`: Search/Fetch can try another eligible account after a complete,
  structured HTTP 401/403/429 rejection without usage. Attempts are bounded to
  three accounts and share a single deadline and ordinary conversation metadata.
  Each attempt has its own WAL intent and settlement. A rejected 401 credential
  is disabled before failover; 429 updates cooldown. No failover for 11128,
  11140, reported usage, malformed/truncated replies, network ambiguity, or 5xx.
  These deliberate limits are stricter than upstream's broad tool retry loop.
- `a2ad763`: optional response identity restoration in Chat Completions,
  Responses, and Messages, for streamed and aggregated output. The plan belongs
  to the original request, uses only explicit instruction-role declarations and
  configured literal identity rules, and is unchanged by retries/config reload.
  Only visible text and reasoning text are restored; refusal, tool IDs, names,
  arguments, usage, URLs, paths, and identifier substrings are preserved.
- `a2ad763`: model list, rate details, and catalog scheduling policies share one
  price resolver. Actual usage proves only free/paid, never a numeric multiplier.
  Latest valid observations survive log cleanup and restart in the checksummed
  catalog snapshot. Identical price refreshes retain prior change timestamps;
  new official rules invalidate older evidence. Promotions are checked at read
  time, including start/expiry boundaries and missing-base-price cases. Saved
  probe verdict timestamps remain separate from failed attempt reservations.
  International `-sg` catalog IDs have a display alias while details retain the
  original ID; client-requested model names are not rewritten.
- `a2ad763`: HTML-escaped JSON highlighting in config preview/editor with
  independent text editing, scrolling, selection, and source preservation.
  Rate detail caches are invalidated when source/promotion revisions change.
- `e060d95`: only an HTTP 429 can establish a model rate cooldown. A 500/11134
  suggestion to switch models, or a bare SSE business error without HTTP 429,
  leaves rate cooldown unchanged. Error completion and usage settlement remain
  intact.

## Configuration

Response restoration is experimental and **disabled by default**. To opt in,
edit the existing console config's identity section, for example:

```json
{
  "identity": {
    "target": "WorkBuddy",
    "responseRestore": true
  }
}
```

Alternatively use `WORKBUDDY_IDENTITY_RESPONSE_RESTORE=true` in the standalone
environment. An explicit console `false` overrides that baseline. Existing
inbound prompt defaults and scheduling/maintenance defaults are unchanged.
No production config adjustment is required for the other changes.

## Verification

`ci/verify.sh` runs `--upstream-sync-check` alongside existing gateway checks,
seven foundation suites, and frontend tests. New checks use synthetic requests,
local Kestrel listeners, temporary credentials/keys/ledgers, an injected upstream
handler, and a fake clock. They cover 48 HTTP scenarios (three protocols in both
output modes, plus CN/INTL Search/Fetch failures), split-text/Unicode boundaries,
tools, price conflicts, promotions, aliases, and durable evidence. Model runtime
tests add HTTP 500/11134 cases across all protocols and both output modes.

These are protocol tests, not validation using an installed DSH/Codex client or
real upstream models. They do not establish a fix for upstream channel admission
restrictions. No production deployment or real model request is part of this PR.

The PR is stacked on `codex/linux-native-aot` (PR #53), because main does not yet
contain the C# gateway. Review this sync separately before merging/deploying.
