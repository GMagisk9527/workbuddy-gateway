# HY4 cache observations and C# conversation metadata

## Scope and evidence

The user reported low `hy4free` cache usage in DSH through this gateway and referenced [dsh-connect-workbuddy issue 224](https://github.com/dingminhua/dsh-connect-workbuddy/issues/224). Its author identifies different conversation-ID lifetimes in the plugin/client, but explicitly reports no A/B experiment; that issue does not establish causality or guarantee a cache-hit improvement here.

A read-only inspection of the retained production statistics found 283 completed `hy4free` samples with 18,814,275 total input tokens and 1,693,568 cached tokens (9.00% of input). Retained `deepseek-v4.1-flash` samples measured 98.29%. The latest HY4 records included approximately 122–130K input tokens with 0–2,112 cached tokens. These are historical observations across an existing rolling window, different conversations and potentially different gateway revisions, not a controlled model comparison. No prompt, API key, credential, raw session ID or tool arguments were exported.

The console and credit ledger already read explicit upstream cache usage. Thus these low values are not explained solely by a percentage display formula. No cache hits should be inferred from local prefix similarity or fabricated when the upstream reports zero.

## DSH 0.2.2-rc.1 client-side prerequisite

The user confirmed DSH `0.2.2-rc.1`. The corresponding published `@deepseek-ai/dsh-llm-pi-ai` package declares `@earendil-works/pi-ai: ^1.3.0`. Its compatibility gate withholds `sendSessionAffinityHeaders` and `sessionAffinityFormat` from configurable profiles. Adding `compat.sendSessionAffinityHeaders: true` to a profile is therefore not a supported setting in that release.

The verified minimum matching dependency, pi-ai `1.3.0`, sends no session-affinity headers for a generic third-party OpenAI Chat provider by default. It does send `session_id` for an OpenAI Responses call with a session ID and the default OpenAI session-affinity format. DSH passes its actual `options.sessionId` into the adapter; the configurable static header map alone cannot generate that per-conversation value. The installed dependency lock and any custom header-contributing plugins were not inspected, so published-source defaults do not prove that every live request lacks the field.

For this DSH version, selecting `openai-responses` as the gateway provider's API is the available protocol-level path to transmit its real session identity without a fixed shared header or an unsupported compatibility override. Keep the existing endpoint, credentials and model. Actual client traffic, reverse-proxy preservation of `session_id`, and HY4 cache improvement still require a live DSH conversation after deployment. The gateway intentionally continues creating independent correlation IDs for Chat requests without an identity; it cannot safely infer which identical prompts belong to the same private conversation.

pi-ai `1.3.0` also reads Chat cache usage from standard nested `prompt_tokens_details.cached_tokens`, then vendor top-level `prompt_cache_hit_tokens`/`cached_tokens`; its input counter excludes cache reads and writes. The gateway history input counter contains the whole upstream prompt count, so compare `cached / prompt` in gateway history against `cacheRead / (uncached input + cacheRead + cacheWrite)` on that client, using the same requests. Different windows or denominators cannot establish a cache defect.

Package integrity values and inspected source hashes are retained in [the provenance record](benchmarks/dsh-0.2.2-rc.1-session-provenance.json). These source checks do not substitute for a real DSH run.

## Active reverse-proxy prerequisite

A read-only `nginx -T` inspection found the active API virtual hosts include the C# proxy snippet, but define neither `underscores_in_headers on` nor `ignore_invalid_headers off`. Nginx defaults to rejecting headers with underscores and ignoring invalid headers ([core documentation](https://nginx.org/en/docs/http/ngx_http_core_module.html#underscores_in_headers)). Thus switching DSH to Responses alone does not complete identity propagation through the current ingress: its `session_id` field also needs to survive Nginx.

A candidate adding only `underscores_in_headers on;` to the existing gateway-only shared server snippet passed `nginx -t` in a private mount namespace. The active snippet hash was verified unchanged afterward; production was not reloaded. This is a production configuration adjustment to apply with the authorized rollout, scoped to the API virtual hosts, not a gateway JSON setting. CDN preservation of the header and an actual DSH conversation still need end-to-end confirmation. No fixed session ID, official-client identity, alternate backend request or paid model call is needed to correct this ingress behavior.

## Confirmed migration defects and fix

Go's `conversation.go` prepares `X-Conversation-ID`, `X-Conversation-Request-ID` and `X-Conversation-Message-ID` before protocol conversion and retains them through internal retries. The C# direct upstream path previously applied account authentication and trace headers but did not attach these conversation fields. Its local account affinity used a client-session digest independently. Local account affinity alone does not transmit session identity to the upstream.

`ConversationHeaders.Prepare` now reads the original validated request during the existing adapter inspection callback, before Chat/Responses/Messages conversion can drop metadata. It derives a bounded, caller-scoped UUID from the same session digest used by account affinity. It does not store the raw client session. Accepted sources retain the existing priority: explicit conversation header, Codex `thread-id`, session header aliases including DSH `session_id`, Claude session/agent headers, top-level identity fields and metadata fields.

The derived C# UUID uses the existing namespace with the C# caller/session digests; exact generated values are not promised to equal the Go implementation's values. Stability and caller/agent isolation are preserved. No account, model or prompt is used to guess a session. With no valid client identity or no authenticated caller, each independent HTTP request gets a fresh conversation. Prefix affinity retains its existing behavior and does not become a shared upstream conversation.

Explicit turn/message correlation fields are accepted from the corresponding headers or body/metadata fields and normalized to UUIDs where necessary. A fresh independent HTTP request gets new default turn/message IDs; clients that supply a stable user-turn ID retain it. The gateway does not infer user turns from tool history. The tuple is created outside account selection and internal retry loops, so definitely-unsent transport retries and the existing restricted-account fallback retain it. A client-created replacement HTTP request can supply the same explicit fields if it needs logical retry identity.

This adds ordinary session correlation metadata. It does not add an official-client signature, device token, client ID, cookie or identity claim. Clear upstream channel restrictions remain errors.

Separately, `SessionAffinity.CachedTokens` previously recognized only nested detail fields and `cache_read_input_tokens`, while the protocol/ledger readers also recognized `prompt_cache_hit_tokens` and `cached_tokens`. These aliases now feed the existing cache-evidence grace rule consistently. Positive observed evidence takes precedence over a zero alias; unknown/invalid observations remain unknown. This does not alter scheduling policy, cache-hit counts sent to clients, prices, credits or WAL format.

## Verification

- Regression before the fix: affinity tests fail on an explicit `prompt_cache_hit_tokens` observation.
- 367 new offline checks cover real gateway HTTP paths across all three protocols, both CN/INTL endpoint selections, streaming/aggregate responses and affinity on/off. They verify session continuity after synthetic compaction, user-code preservation, three-protocol tool-call association/order, caller/agent isolation, independent anonymous requests, exact upstream call counts and zero-credit ledger settlement. No real upstream is contacted.
- The existing affinity HTTP cases additionally verify stable upstream conversations, fresh independent turn/message IDs, caller isolation and unchanged IDs during account fallback. The strengthened suite passes 822 checks.
- A definitely-unsent connection-refusal retry test verifies that the cloned second upstream request retains all three identifiers. Native gateway smoke tests require valid correlation UUIDs on model requests.
- All 552 protocol output/error fingerprints remain identical after rebuilding against the final assemblies. The final code revision `3016cb5` passed all five standard checks, including Linux full-gateway Native AOT, native components, JIT/foundation/frontend regressions, Go vet/test/race and TCP faults.
- Invalid Unicode, oversized/control-character identity values, duplicate headers, body/metadata sources, explicit turn/message replay and conflicting cache-usage aliases have targeted coverage.
- A subsequent documentation-only CI run exposed a timing race in the existing cooldown test: its assertion could miss a 300 ms replay window. The release-before-wait assertion now uses the real two-minute cancellation case with a bounded condition wait. All other cases still check persisted restriction/settlement after completion, and the 280 ms minimum replay-delay check remains. The suite retains all 99 cooldown assertions; production replay code is unchanged. Two local full failover/cooldown runs passed after this test-only repair.

No new real `hy4free` requests or DSH-client A/B experiment were performed for this change. A real cache-rate improvement remains unverified. The earlier authorized free `hy3` tests concern the previous performance revision and do not establish HY4 cache behavior. Production has not been deployed or reconfigured by this fix; historical counters and ledger data were not rewritten.
