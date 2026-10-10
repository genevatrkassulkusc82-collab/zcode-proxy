# ADR 0001: zcode-proxy stays a standalone application (OmniRoute in-process merge rejected)

Date: 2026-10-09
Status: Accepted
Revisit: see "Flip conditions" below

## Context

zcode-proxy (this repo, ~44.5k Go LOC, 183 test functions) fronts the z.ai / GLM
coding plan: multi-account pool, OAuth lifecycle, captcha auto-solving
(rod + chromium), quota reset ledger, gateway key distribution, embedded admin
panel. OmniRoute (diegosouzapw/OmniRoute, Node/TS, 127.0.0.1:20128) is the
multi-provider gateway. OmniRoute already consumes this proxy as the `zcproxy`
custom node — data-driven passthrough, zero special-casing in OmniRoute —
as the paid-last leg of the `free-first` combo.

Question reviewed 2026-10-09: fold this code into OmniRoute as a native
provider instead of running a separate application?

## Decision

Keep the two-process architecture. The custom-node bridge IS the integration;
an in-process port was evaluated and rejected.

## Reasons

1. **No landing point.** OmniRoute executors are compile-time
   (`open-sse/executors/registry.ts`, 159 files); its plugin API is hooks-only
   (`src/lib/plugins/` — can modify bodies, cannot register executors, auth
   flows, or routing). Landing code means brittle minified-dist patches wiped
   by every nightly update, or a deep fork forcing full-monorepo builds — a
   path already rejected for a much smaller patch (stepfun).
2. **The port is a rewrite, not a merge.** Go → TypeScript across a ~7.5k-line
   protocol core (convert.go 1887L, relay.go 1595L, api_handlers.go 1274L,
   captcha.go 765L) plus 12 rounds of soaked ZCode wire-compat fixes (envelope
   vocabulary, 32MB cap, keepalives, 529 mixed-failure handling, ms-precision
   reset normalization).
3. **The hard layers lack Node equivalents.** utls TLS fingerprinting has no
   maintained Node port; the rod/chromium captcha pipeline (CF/WAF challenge
   isolation, solve budgets, audit hardening) is more developed than any TS
   equivalent found in OmniRoute's executors.
4. **Altitude mismatch, not redundancy.** OmniRoute has cousins (account
   rotation, quota-reset tracking, egress proxies) but at passthrough altitude;
   nothing upstream-protocol-aware, and no dollar-spend enforcement anywhere
   (`src/lib/spend/batchWriter.ts` is accounting only). This proxy's ledger is
   the only spend guard on the z.ai path.
5. **Blast radius.** Separate processes keep captcha/chromium memory spikes and
   relay bugs from taking down OmniRoute's other ~20 providers, and deploys
   stay independent.

Upstream lever filed instead: OmniRoute issue #16025 proposes first-class
external (out-of-process) executors with pool-status ingestion — the generic
form of what this proxy needs from a gateway. If accepted upstream, the seam
deepens without any merge.

## Verification evidence (2026-10-09)

- zcproxy node `openai-compatible-chat-b454d3ea…` → `http://127.0.0.1:8687/v1`,
  connection active, backoff 0, no errors, models synced 2026-10-08 16:22.
- `free-first` combo composition intact: glm/glm-5.3-flash → glm/glm-5-turbo →
  deepseek/deepseek-v4-flash → zcproxy/GLM-5.3 (priority drain-in-order).
- 135 lifetime calls routed through the seam; last one 2026-10-09 04:37 HKT
  (= 2026-10-08 20:37 UTC), matching gateway key `omniroute-local`
  last_used_at — that call 429'd upstream, and the account entered designed
  cooling until 2026-10-09 22:26 HKT.
- Gateway catalog serves 2 zcproxy models (GLM-5.3, GLM-5.3-Flash) — narrowed
  by auto-fetch from the 9 originally imported; the combo needs GLM-5.3 only.

## Flip conditions

Revisit a TS rewrite inside an OmniRoute fork only if: the zcode-proxy upstream
(genevatrkassulkusc82-collab) dies and the 10-PR contribution series goes
stale; or a z.ai wire change makes maintaining the Go fork cost more than a
rewrite. Not before.

## Edge-dedup note (not done, deliberately)

If ALL z.ai traffic ever routes through OmniRoute, this proxy's gateway-key
layer partially overlaps OmniRoute's api_keys/quota tables — the one honest
simplification lever. Shrink at the edges if that day comes; do not merge
applications.
