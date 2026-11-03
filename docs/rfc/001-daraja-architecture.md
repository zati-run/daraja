# RFC 001: Daraja architecture

- **Status:** Draft, revision 9. Not accepted.
- **Date:** 2026-10-31
- **Owner:** David Ndungu
- **Repository:** `github.com/zati-run/daraja`

## How to read this RFC

Every statement carries one of three labels. A reader must not treat a lower
label as a higher one.

| Label | Meaning |
|---|---|
| **[Decision]** | The owner decided this. Each decision has an ID (`D-n`) and a date in [Owner decisions](#owner-decisions). |
| **[Proposal]** | The author proposes this. It isn't accepted until the owner accepts it or until the evidence listed for it proves it. |
| **[Evidence]** | A behavior observed in a recorded run, with environment and method. See [Evidence status](#evidence-status). |

An RFC isn't proof that a design works. A dependency being present isn't proof
that a capability is qualified. A generated artifact isn't proof that it runs.

### Revision 4 changes

The owner answered the questions that blocked acceptance on 2026-10-31:

- **D-13:** Daraja supersedes EAAP. "Daraja is the future. It is what EAAP
  was supposed to be and failed." The proposed EAAP split (U-7) is withdrawn.
  Daraja owns discovery as well as execution.
- **D-14:** The product target is an application's **unofficial, internal
  API**: the same-origin endpoints its own web client calls with the user's
  browser session. The first target is Zendesk's agent UI traffic. Zendesk's
  official, documented API is out of scope, because "we do not need Daraja to
  use that". Rung 1 (`http.json`) running inside the page is therefore the
  primary path, and the other rungs remain on the ladder.
- **D-15:** Agents reach Daraja over Streamable HTTP MCP, authorized with
  OAuth 2.1, and clients identify themselves with a **Client ID Metadata
  Document (CIMD)**. A static, shared API key isn't the consumer interface.
- **D-16:** The headed browser capability runs on the owner's Mac.
- **D-11 reaffirmed:** the RFC and the implementation land in one pull
  request at the end.

As a consequence, this revision moves `http.form_html` and `browser.steps`
from phase 1a to phase 2 (a **[Proposal]**). They stay in the artifact format
and on the ladder, but v1 doesn't implement them. See [Phased scope](#phased-scope).

### Revision 9 changes (`browser.steps`)

`browser.steps` is implemented as [Integration artifact](#integration-artifact)
describes, with these **[Proposal]** details:

- **Steps** are `navigate`, `fill`, `select`, `click` (optionally with a `text`
  anchor), `wait_for` and `wait_path_not`. It needs the `browser` session
  provider.
- **Actions are scripted in the page.** `fill` sets a text field's value and
  fires `input` and `change`, `select` does the same, and `click` calls the
  element's `click()`. No key press or mouse event is sent, so a value can't
  submit a form, and focus or scrolling can't send an action elsewhere. An
  application that checks `isTrusted`, or that reads key or mouse events, won't
  respond. After a commit step, that's `uncertain`, and reconcile settles it.
- **Targets.** A step acts on the first shown, enabled match. A `click` also
  needs the element's center to land on it, so a covered element isn't clicked.
  The origin and the step's `expect` are read in the same synchronous evaluation
  that finds the element and acts on it.
- **Isolated world.** Every check and action runs in a fresh isolated world of
  the current document, so a page that replaces a built-in can't change what they
  see or return, and a navigation makes the call fail rather than run elsewhere.
  The final page and its URL are read in one such evaluation. The page's own
  handlers still run, and they can do anything the page can.
- **At most once.** Only looking for a target is retried. If the evaluation that
  acts fails, the action may have run, so it isn't repeated.
- **Commit step.** A write has exactly one `commit: true`, on a `click` or a
  `navigate`. A failure before it, or in its own `expect` check, is
  `failed/not_sent`, because the commit action hasn't started. Any failure once
  that action starts is `uncertain`. A declared refusal on the final page is
  `failed/sent`. An anchored commit step needs `expect`. The author declares
  that steps before the commit step have no effect; Daraja can't check that.
- **Reads.** A read that fills, selects or clicks needs `reviewed_safe_method`,
  as a read using an unsafe HTTP method does. A read made of `navigate` and
  waits doesn't.
- **Drift guard.** Every step's `expect` and origin are checked before it acts,
  also after the commit step, retried until the step's timeout (15 s) because
  pages render late. A `navigate` checks only `expect` and a `wait_path_not`
  checks them apart from what it waits for, since neither acts on an element. A mismatch or a selector that never appears is
  `source_changed` before the commit action and `uncertain` after it, and the
  host drops its cached session, because the page may be a sign-in page.
- **Outcome.** The answer is the final page and its URL. `response.extract` and
  `response.refused_when` read them with the same code as `http.form_html`, and
  a write must declare `response.identifier`. No identifier and no declared
  refusal is `uncertain`.
- **Proof of the write.** An application can end on a page that has nothing to
  do with this write, so the identifier must come from a CSS extract whose `text`
  anchor shows what the write typed: `{{receipt.marker}}` for a marker-reconciled
  write, an `{{input.x}}` value otherwise. An input value is only as unique as
  the caller makes it. A marker-reconciled write must type the marker with a
  `fill` before the commit step. This means the application must show the marker
  on the page the plan ends on.
- **Failure text.** Scripts answer with fixed codes. A failed step reports its
  step number and what the artifact declared, never a browser or script message,
  because those can hold page text.

Not done: no write has run through `browser.steps` on a real application.
`fill` and `select` target form fields; a rich-text editor isn't tested, and
`fill` refuses an element without a text `value`. A plan can't tell a missing
record from a redesign, so both report `source_changed`. Page dialogs aren't
handled, so one blocks the plan until the step's timeout. The planner doesn't
propose this kind.

### Revision 8 changes (`http.form_html`)

`http.form_html` is implemented. These are **[Proposal]** changes the owner
hasn't separately decided, and the details below replace the sketch in
[Integration artifact](#integration-artifact) where they differ:

- **Reads** fetch a page and take outputs from `response.extract`: a CSS
  selector with an optional attribute and text anchor, an anchored
  `location_regex` (a write's redirect only, with its anchors applying to the
  whole pattern), or a JSON Pointer. A text anchor can use `{{input.x}}`, so
  a reconcile read can find a marker in an HTML list.
- **Writes** must use POST, declare `request.form_page` and fetch it first. The submitted
  values are the form's own, in page order, including its CSRF token, with
  `request.form` set over them. They follow the common rules a browser applies: form ownership,
  disabled fieldsets and options, drop-down defaults, radio groups and textarea
  newlines. Daraja doesn't reimplement how browsers sanitize values. A default
  it can't reproduce exactly is drift unless the artifact sets that field: for
  example a number, email, date or list-box control, a text value with a newline,
  `_charset_`, `dirname`, a file control, or a hard-wrapped textarea. So is a
  form whose ID isn't unique. A form a page encodes as
  anything but UTF-8, by its `Content-Type`, `<meta>` or `accept-charset`, is
  drift.
  A page that changes values with script at submit isn't covered. Declaring a
  name that a button with its own `formaction` or similar also uses is drift.
  The action must match the
  path and query that will be sent, and a URL a browser and Go read differently,
  with a backslash, a space or `%2e`, is drift. A page over 8 MiB is drift, because a cut-off
  page could lose a field.
- **Drift is checked before sending.** The selector must match exactly one
  form, and its method, action and encoding must match the artifact, and every
  declared field must exist. Otherwise the write is `failed/not_sent` with
  `source_changed`. Only `application/x-www-form-urlencoded` is supported.
- **A form write must declare `response.identifier`,** naming an extract. A
  status alone can't show that a form write worked, because most sign-in and
  validation pages answer 200.
- **Post-redirect-get.** `response.redirect_success` is an anchored pattern for
  a local path and query. A redirect matching it is success. Over HTTP, a body that
  also matches `refused_when` makes it `uncertain`; in the browser a redirect's
  body can't be read, so this isn't checked there. Any other redirect stays
  `uncertain`. A redirect to sign in isn't success unless the reviewed pattern
  matches its path, so the pattern must exclude such routes: `^/notes/[0-9]+$`,
  not `^/notes/[^/]+$`. A form operation can't list a 3xx in
  `success_status`. A relative `Location` such as `notes/7` isn't read, because it
  depends on the request's path, so that write stays `uncertain`. In the page,
  `fetch` can't read a redirect's target, so the capability reads it from the
  browser's network events. If it can't tell the answer apart, the write stays
  `uncertain`.
- `response.refused_when` also takes `css`, for a validation error on a page.
  It applies to a status in `success_status`, so a form that answers 200 with its errors must list 200.
- **`placement` isn't a field.** The session provider decides where an
  operation runs.

Not done: the deterministic planner doesn't propose this kind, so these
artifacts are written by hand. No real application has been qualified with it.

### Revision 7 changes (A14 follow-ups)

These are **[Proposal]** changes the owner hasn't separately decided:

- **Declared failures.** `response.refused_when` lists answers that are
  definite failures despite a success status. Each entry has a `json_pointer`
  and exactly one of `equals` and `contains`. A matching write is `failed` at
  once, and a matching read returns `source_refused`. An answer that matches and
  also carries the declared identifier contradicts itself, so the write stays
  `uncertain`. `equals` compares a number as sent, and `contains` applies only
  to a string. An answer that matches no entry is handled as before: without
  its declared identifier the write stays `uncertain`, and if the operation
  declares no identifier, a success status records `succeeded`, so such a write
  can't detect a failure nobody declared. If it declares an output or
  `refused_when`, a body that doesn't decode stays `uncertain`. A reconcile read
  must map an output. A read that declares an output or `refused_when` must
  answer with exactly one JSON value; a read that declares neither isn't parsed.
- **Known gaps, not addressed here.** A reconcile read can take an undeclared
  error sent with a success status for absence (issue #5). Go's transport can
  resend a GET write on a reused connection (issue #6).
- **Revision 6's example was wrong for this case.** A note to a missing ticket
  doesn't answer `CreateOrUpdateTicketFailure`. It answers HTTP 200 with `data`
  null and a generic `INTERNAL_SERVER_ERROR`, and the `RESOURCE_NOT_FOUND` code
  is inside `/errors/0/extensions/response/body`, which is why `contains`
  exists. The generic code alone isn't a declared failure, because a server
  error doesn't show that the write wasn't applied.
- **Client version.** No code change. The pinned `apollographql-client-version`
  had already drifted (`v31906` in the artifact, `v31947` live, two days later).
  The artifact now reads it at session time through the existing `html_input`
  source (`<input name="app_version">` on `/agent/dashboard`) and sends it from
  `session.request_headers`. Whether Zendesk rejects a stale version wasn't
  established.

### Revision 6 changes (A14 qualification)

Qualifying Zendesk (A14) exposed two explorer gaps. Both are fixed, each with
a test that fails on the earlier code. These are **[Proposal]** changes the
owner hasn't separately decided:

- **GraphQL reads in a read-only walk.** Zendesk's agent UI reads through
  `POST /api/graphql`. The write guard blocked every POST, so a read-only walk
  never loaded ticket data. A task may now name `graphql_read_paths`. A POST
  to one of them goes through only when every operation in its body is a
  `query` (a small lexer checks each top-level definition, and skips strings
  and comments). A mutation, a subscription, a persisted query sent by hash
  only, or anything that doesn't parse stays blocked. Observations on these
  paths are grouped by operation name and keep the query document with every
  inline string and number literal replaced by a placeholder, never variable
  values. A body or URL a server could read differently from the guard (a
  repeated key, a `Query`-style case variant, a repeated `query` parameter,
  or a POST with any URL query string) is blocked. So is any request, on any
  path, that carries a method-override header or a `_method` parameter.
- **Every tab is recorded, and every request is guarded.** The recorder
  watched only the tab it opened. In the owner's first demonstration the
  ticket was created in another tab and nothing was recorded. Worse, in a
  read-only walk a write from a second tab skipped the write guard: the
  regression test created a note through it.
  - Recording: every page tab opened during an exploration is attached and
    recorded. A request a tab sends before it's attached isn't recorded,
    and the report names this gap whenever another tab opened.
  - Guarding (after peer review R1/R2): the write guard moved from each tab
    to the browser target. In a read-only walk Chrome pauses every request,
    from every target and of every resource type, to any origin, before it
    leaves. Only GET, HEAD, OPTIONS and query-only GraphQL POSTs continue.
    On a GraphQL read path, a GET or HEAD passes only when it carries a
    `query` parameter that is query-only. Some servers accept mutations over
    GET, and a persisted query sent by hash only (Apollo APQ) can't be
    checked, so both are blocked. OPTIONS preflights pass. Regression
    tests, each with a control that shows the write lands when writes are
    allowed, block a form posted to a new window, a new window that writes
    at once, a dedicated worker, a beacon, a GraphQL mutation over GET, a
    hash-only persisted query over GET, an `<a ping>`, a service-worker
    fetch and a WebSocket.
    On the per-tab guard the form, new-window, beacon and GET-mutation
    writes reached the app. The beacon hole affected the main tab too.
  - **WebSockets:** request interception doesn't see socket frames, and
    `Network.setBlockedURLs` didn't stop a WebSocket handshake in this
    Chrome (tested in three pattern forms). In a read-only walk, each
    attached tab disables the `WebSocket` and `WebTransport` constructors
    before any page script runs, and refuses `SharedWorker` (tab-level
    auto-attach doesn't reach shared workers, and browser-level auto-attach
    stalls popups). Each attached tab also auto-attaches its workers and
    cross-site frames paused, before their first script runs:
    - A dedicated worker has its socket constructors removed, checked by
      constructing one, and its own workers armed the same way. Then it is
      resumed.
    - A cross-site frame (it has no execution context while paused) and a
      service worker (attaching one paused hangs) are held paused until
      the walk ends. They never run, and the report names them as a gap.
      Tabs bypass service workers, so a page doesn't wait on a held one.
      Service workers already running are stopped at the start of the walk.
    - A target that can't be armed is held too.
    Regression tests, each with a writes-allowed control, block a WebSocket
    opened from a dedicated worker, a nested worker, a shared worker and a
    cross-site frame; without the fix each one reached the app.
    **Known exposure:** a document, worker or frame that was already running
    when its tab was attached keeps its socket constructors. The report
    counts those, states that socket frames are neither observed nor
    checked by the write guard, and counts any socket the page opened.
- **A read-only walk ends by stopping Chrome.** CI found this. When a read-only
  walk ended, its write guard ended too while its pages kept running until the
  browser closed, so a timer could write, and a held frame was released to load
  with its sockets. Now the walk kills the Chrome process while the guards still
  hold, so no page, frame, worker or pending navigation outlives them. Cleaning
  up page by page isn't enough: three rounds of review each found something it
  missed, such as a popup navigating its opener during cleanup. The profile stays
  on disk, and a caller opens it again to use it. Runs before this fix had that
  window between the walk's end and the browser closing, including the walk
  recorded below.
- **A14 follow-ups** (peer review, low). Two are resolved; see
  [Revision 7](#revision-7-changes-a14-follow-ups).
  - Writes fire the source's own triggers and notifications (for Zendesk,
    triggers and automations). `destructive` doesn't describe that kind of
    side effect. Reviewers should check the source's automation before
    publishing a write.

### Revision 5 changes (peer review)

A read-only peer review of revision 4 and its code (2026-10-31) found three
high-severity defects. They are fixed in the implementation. Each fix has a
regression test, and each test was confirmed to fail on the earlier code.
These are **[Proposal]** contract changes that the owner hasn't separately
decided:

- **Operations never follow redirects.** A write sent with an expired session
  could follow a same-origin redirect to the sign-in page, get its 200, and
  be recorded as `succeeded`. Now the runtime and the in-page executor don't
  follow redirects for operations (`redirect: "manual"` in the page). A
  redirected write is `uncertain` and drops the session. A redirected read
  refreshes the session once. A success status without the declared response
  shape (an identifier when one is declared, otherwise JSON when output is
  mapped) is `uncertain`, not `succeeded`. A declared redirect success shape
  (post-redirect-get) is deferred to phase 2.
- **Receipts belong to a stable identity.** Every OAuth consent creates a new
  grant. If receipts had been keyed by grant, a re-authorized client could
  have re-sent an uncertain write. Receipts now belong to the client
  (`client_id`) within one credential binding. A CIMD `client_id` names
  software, not an instance, so two instances of one client under the same
  owner and binding share receipts. Each receipt therefore stores a hash of
  the validated input. The same key with the same input is one call and
  replays. The same key with different input returns
  `idempotency_key_conflict`: nothing is sent, and nothing from the other
  receipt is returned. `daraja__receipt` and `daraja__reconcile` look up by
  key alone, so two instances that reuse a key can see each other's receipt
  for it.
- **The capability authenticates its runtime.** The runtime URL is where the
  capability takes orders that run as the user. It must be https, or
  loopback http. `-runtime-pin` (a public-key SHA-256) trusts only one
  runtime. `-allow-insecure-runtime` exists for an isolated lab network, and
  the A13 lab run used it.
- Medium fixes:
  - A single shared response-size limit. Results up to 8 MiB are delivered.
  - Consent passphrase failures are limited across all requests. After 10 in
    15 minutes, consent locks, before any key derivation runs.
  - The pending and client caches are bounded.
  - The CIMD fetch refuses shared address space (tailnets), benchmarking,
    documentation and NAT64 ranges, and no longer echoes upstream status.
  - Reuse of a rotated refresh token revokes the grant (RFC 9700).
  - A `not_sent` failure says it's safe to retry with a new key.
- **Custody gate:** where file modes can't be checked (`-unikernel-file-modes`,
  for example credentials built into a Nanos image), the runtime refuses any
  credential binding not marked `synthetic`.
- **A person's sign-in step is left alone.** While the page is still on a
  sign-in step left to a person (for example, an emailed passcode), the
  capability neither fetches nor navigates in that window. It answers
  `session_needs_human` until the page leaves the step. The guard fails closed: if the page's location can't be read (for
  example, the browser died), the binding keeps answering
  `session_needs_human` until the capability restarts.
- **Timeout ordering:** in-page fetch 45 s < capability job 60 s < runtime
  broker (`-capability-timeout`, 90 s) < the MCP client's patience. Both
  processes log their timeouts at start. The runtime refuses a
  `-capability-timeout` that isn't greater than `-request-timeout`. The
  ordering across processes is checked only by these settings, not by a
  test.
- **Deferred, recorded here:**
  - A two-phase job claim, so a dropped long-poll delivery is `not_sent`
    instead of `uncertain`. Today such a write is uncertain, not wrong, and
    reconcile resolves it. **Condition:** every write in a qualified
    artifact, including A14's, must declare `marker` reconciliation. A write
    that can't declare it makes the two-phase claim blocking for that write.
  - A dedicated browser target per binding.
  - **Known exposure:** the in-page executor runs in the page's main world,
    so the source's own page scripts can observe or alter Daraja's requests
    and answers. The isolated-world executor is deferred.
  - Runtime-signed jobs and a declared redirect success shape: phase 2.

### Revision 2 changes

Revision 1 described, in effect, an *API recorder*: every execution path
assumed the application had a hidden same-origin JSON API. A peer review (the
`daraja` Claude session, 2026-10-31) and the owner's correction ("the goal is
not to connect Zendesk but to create a tool that provides a bridge to connect
to apps without APIs") made that the central defect. Revision 2 makes these
structural changes, all **[Proposal]** until the owner accepts them:

1. Operations have an **execution kind** from a ladder: `http.json`,
   `http.form_html` and `browser.steps`. Browser-executed operations are
   first-class in v1, not phase 2.
2. An explicit **execution capability** contract replaces the unnamed
   runtime-to-browser side channel. The runtime stays the only scheduler and
   receipt journal.
3. By default **the session stays in the browser that owns it**, and the
   browser executes. Leasing cookies to a remote runtime becomes an opt-in,
   per-origin, qualified exception.
4. The v1 authoring claim becomes **demonstration-driven**: a person performs
   the task once in a headed browser, and Daraja generalizes the recording.
   Natural-language authoring comes after v1.
5. The overlap with EAAP and the missing portfolio row are now owner
   questions that block acceptance.

## Summary

Daraja is an independently usable Go system that bridges applications with no
usable official API to generic MCP consumers. **[Decision D-14]** What it
bridges is the application's unofficial, internal API: the same-origin
requests the application's own web client makes with the user's session. It turns a bounded task, demonstrated once
by a person with their own authority, into a reusable, scoped integration. The
operations are typed, reviewed and qualified, and they execute without a model
call on each request. Daraja connects through the same user login a person
uses.

The parts stay separate:

1. **Builder:** records a demonstration (UI steps and the network traffic they
   cause) in a headed host browser, proposes operations, and records
   provenance.
2. **Qualifier:** checks a candidate integration against the live
   application, within the authority the owner granted, and records evidence
   for each operation and execution kind.
3. **Runtime:** schedules every call, keeps the one receipt journal, and
   serves MCP. It runs as one process on Nanos. It executes `http.*`
   operations itself and brokers `browser.*` operations to a registered
   execution capability.
4. **Execution capability:** a separately qualified process that owns a
   user session and executes operations for the runtime. The first one is a
   host-side headed browser. A native desktop capability may follow, but this
   RFC doesn't claim desktop support.

## Product intent

### What Daraja does

- A user demonstrates a bounded task against an application in a headed
  browser, for example "open a ticket and add an internal note".
- Daraja records what happened: the UI steps, the fields the user typed, what
  the page showed afterwards, and any same-origin requests the page made.
- Daraja proposes an explicit integration: named operations with input and
  output JSON Schemas, an execution kind, an effect class that a person
  confirms, a reconciliation strategy for writes, and provenance.
- A reviewer approves it, and the qualifier checks it live.
- The runtime serves the operations as MCP tools, and each call runs
  deterministically.

### Execution-kind ladder

**[Proposal]** Each operation declares exactly one execution kind. The builder
proposes the lowest rung that qualified for that operation.

| Rung | Kind | When it fits | Runs in |
|---|---|---|---|
| 1 | `http.json` | The page itself calls a same-origin JSON endpoint, and the session can be used from the executor | Runtime (Nanos), or inside the page through the browser capability |
| 2 | `http.form_html` | Server-rendered forms and links; the result is read from HTML with declarative CSS selectors, from the redirect target, or from a page re-read afterwards | Runtime (Nanos), or inside the page |
| 3 | `browser.steps` | Anything that needs page JavaScript: JS-signed requests, rotating nonces, client-side state, visual-only flows | A browser execution capability, never Nanos |

Rules:

- Rungs 1 and 2 are substitution-only templates plus declarative extractors.
  They carry no code.
- Rung 3 is a declarative UI plan: `navigate`, `fill`, `click`, `select`,
  `wait_for`, `extract`. Selectors carry a text-anchor fallback. A plan holds
  no script and no `eval`.
- **The effect class is never inferred from the HTTP method alone.** Legacy
  apps use GET links for actions, and many SPAs use POST for searches. The
  planner proposes an effect, and review must confirm `effect` and
  `destructive` for every operation. A write without an explicit `destructive`
  value is invalid.
- A rung-1 operation that runs inside the page through the browser capability
  gets the page's own signing and cookies for free. This is the default
  placement whenever the session can't leave the browser.

### What Daraja is not

**[Decision D-1]** Daraja isn't a CRM, an accounting product, a domain
workflow engine, a Zati-specific adapter, an identity provider, or a
replacement for the source application's authorization. The application is
the authority for its data and actions.

- The core holds no industry, department, application, workflow name or
  tool switch. Application-specific details live in the artifact or in an
  external adapter.
- Integrations are usable without Zati.
- Nothing confers authority: not a model response, an observed page, an
  inferred schema, a catalog listing, or **holding a browser session**.
  Execution authority comes only from an owner grant for each integration,
  operation and execution capability.
- Daraja doesn't claim compatibility with arbitrary applications. Coverage
  and unsupported cases are reported for each qualified integration.

### Inspiration and licensing

**[Decision D-10]** Pomerado (MIT, copyright Pomerado AI, Inc.) is inspiration
only. Daraja is Go only and copies no Pomerado code. Ideas taken, written
independently: task-oriented creation, read/write tool annotations, dispatch
state that separates "not sent" from "possibly dispatched", schema validation
of input and output, and signed list cursors.

**[Decision D-13]** Daraja supersedes EAAP. EAAP remains a source of ideas,
such as bounded, allowlisted, value-free observation and explicit coverage
gaps, and its rule that discovery isn't execution authority. See
[Relationship to EAAP](#relationship-to-eaap).

Ferro becomes a *candidate implementation* of the `browser.steps` execution
capability. Comuse is a possible independently owned `desktop.native`
capability. Neither is a dependency or is qualified by this RFC.

## Ownership

**[Proposal]**

| Concern | Owner |
|---|---|
| Data, actions, permission and authentication of the source application | The source application |
| A user's credentials and live browser session | The execution capability that holds the session, in approved custody |
| Scheduling, the receipt journal, scope and the MCP surface | Daraja runtime (one authoritative journal for each write) |
| Artifact format, builder and qualifier | Daraja |
| Whether to call a tool, and why | The MCP consumer (for example Zati) |
| Network-shape discovery of a browser session | Daraja (D-13: Daraja supersedes EAAP) |
| Browser and native execution engines | Capability implementations (the Daraja host browser first, later possibly Ferro or Comuse), each qualified separately |

## Architecture

**[Proposal]** There are two kinds of environment, and they have different
capabilities. The runtime↔capability protocol is named and authenticated.

```text
 Builder host (headed browser)                     Deployable runtime (Nanos)
 ─────────────────────────────                     ──────────────────────────
 demonstration ─▶ recorder (UI steps + traffic)    integration artifacts (read-only)
                    │ observations (no values)              │
                    ▼                                       ▼
                 planner ─▶ candidate ─▶ review     MCP server ─▶ scope check
                    ─▶ qualifier ─▶ publish ═════▶  receipt journal (before dispatch)
                                                            │
                         ┌──────────────────────────────────┤
                         ▼                                  ▼
               http.* in-process (HTTPS)        broker to execution capability
                                                 (mTLS or scoped token)
                                                            │
                                             Execution capability (host)
                                             headed browser, persistent profile,
                                             owns the user session; executes
                                             http.* in-page and browser.steps
```

### Execution capability contract

**[Proposal]**

- **Direction.** The capability dials out to the runtime and holds one
  authenticated, long-lived stream. The runtime dispatches over that stream.
  The capability may sit behind NAT, for example on a person's computer, with
  no inbound port. A live registration is what reachability means:
  `capability_unavailable` means no stream is registered. A stream that drops
  mid-call is a lost answer.
- `Describe() → {kinds, origins, bindings, artifact hashes, version}`, sent
  when the stream registers.
- `Execute(op_ref, input, receipt_id, marker) → outcome`, where
  `op_ref = {integration_id, version, artifact_sha256, operation}`.
- **The capability is an authority boundary, because it holds the session.**
  - It runs plans only from its own copy of published artifacts.
  - It verifies `artifact_sha256` and refuses unknown hashes, operations and
    origins.
  - It enforces each binding's grant itself, as defense in depth next to the
    runtime's scope check.
  - It never accepts a plan body from the runtime. This is the first concrete
    consumer of artifact signing (U-3).
- **Legal outcomes.** Exactly one of these:

  | state | dispatch | Meaning |
  |---|---|---|
  | `succeeded` | `sent` | The effect is confirmed. |
  | `failed` | `not_sent` | Nothing reached the source. A new key may be used. |
  | `failed` | `sent` | The source definitively refused: a `refused_when` extract matched after the commit step. |
  | `uncertain` | `sent` or `unknown` | The effect may exist. Reconcile it. |

  Any other pair is a protocol error, and the runtime records it as
  `uncertain`.
- The capability holds no receipts. The runtime journals `dispatching` before
  it calls `Execute`, and journals the result afterwards.
- **Sessions and concurrency.**
  - Each credential binding gets one dedicated profile, never a person's
    everyday profile. A binding's session never serves another binding.
  - Calls are serialized per profile with a bounded queue. A full queue
    returns `capability_busy`, which means nothing was sent.
- **Expiry that needs a person.** If re-signing in needs a human step (for
  example an emailed passcode), the call returns `session_needs_human` with a
  reason, and a generic notification hook fires. The capability never blocks
  a call waiting for a person.
- The password the capability uses to sign in again is custody-scoped to the
  capability host (see U-2).
- Each capability and kind gets its own qualification evidence.
- The runtime on Nanos never starts a browser. It serves `browser.*` kinds
  only by brokering, and only if a capability is registered and reachable.
  Otherwise those tools return `capability_unavailable`.

### Builder (host only)

- The builder runs where a headed browser can run.
- **[Evidence]** The first real target refuses headless Chrome and plain HTTP
  sign-in (see Evidence). The recorder and the sign-in capability must
  therefore be able to run headed. This supersedes the "headless" wording of
  D-4. **[Decision D-16]** The long-lived headed browser capability runs on
  the owner's Mac. It dials out to the runtime, so the Mac needs no inbound
  port.
- The recorder records two things: (a) UI steps and the fields the user
  typed, as parameters with no values kept, and the elements read afterwards;
  (b) same-origin request shapes, with method, normalized path, query names,
  status, and JSON or form shape, with no values.
- **Values exist only in recorder memory, for one recording.** To map a
  typed value to a request parameter or a displayed element, the recorder
  correlates salted per-recording tokens of the values in memory. It persists
  only the resulting mapping, then discards the values and the salt.
- **No effect-free guarantee.** The browser can block non-GET requests, but
  that keys on the method, and on apps without APIs a GET link can act. So a
  "read-only" demonstration of such an app isn't guaranteed to be free of
  effects. Demonstrations run only on origins where the owner authorized
  writes, or under a navigation allowlist that a person confirms.
- **GraphQL and tabs.** A task's `graphql_read_paths` lets query-only GraphQL
  POSTs through a read-only walk. Every tab opened during the walk is
  recorded. The write guard runs on the browser target, so it covers every
  tab, worker and resource type (Revision 6).
- Waiting is condition-based (network idle, or an expected request or
  element), not fixed sleeps. A route missed because of a timeout must not be
  reported as "not exercised".

### Planner

- **[Decision D-3]** Pluggable, deterministic first, no hidden model calls.
- **[Proposal]** The v1 deterministic planner *generalizes a demonstration*.
  Typed fields become inputs, read elements become outputs, and the lowest
  qualified rung is chosen. It proposes effect classes and marks each one for
  review.
- It doesn't turn natural-language text into a plan. That's the later model
  adapter. An external authoring agent writing a candidate by hand is the
  near-term natural-language path, and the build report records it as such.

### Review and qualification

- A candidate becomes an integration only after explicit review of names,
  schemas, execution kind, effect, `destructive` and reconciliation.
- The qualifier runs reads live and checks output schemas. It runs writes only
  with owner authorization for that origin. It records dispatch state and
  reconciles.
- Evidence holds operation names, kinds, statuses, timings and schema
  verdicts. It holds no customer data or credentials.

### Runtime

- One Go process, Streamable HTTP and stdio MCP, scope checks, input and
  output validation, and the receipt journal. It starts no subprocess. It
  reaches only declared origins and registered capabilities.

## Web and desktop boundaries

| Capability | Where it runs | Status |
|---|---|---|
| `http.json` | In-page through the browser capability (the default under D-14), or in the runtime when the session may be used there | Both placements are v1 scope. |
| `http.form_html` | As above | Implemented; qualified only on a synthetic application. |
| `browser.steps` | Host browser capability with a headed, persistent profile | Implemented; see [Evidence status](#evidence-status). |
| `desktop.native` | A long-lived native environment that owns permissions and a user session | Not implemented, not claimed |

A visual desktop action isn't an HTTP API. It isn't exposed through OpenAPI
unless the facade preserves identity, session dependence, side effects and
uncertainty.

## Integration artifact

**[Decision D-2]** Declarative, versioned JSON, interpreted by generic
executors. No per-integration code generation.

**[Proposal]** In `daraja.integration/v1alpha1`, an operation adds:

- `execution`: `http.json` | `http.form_html` | `browser.steps`.
- `placement`: `runtime` | `capability`. The default is `capability` whenever
  the session provider is `browser`.
- `extract` (for `http.form_html` and `browser.steps`): named outputs, each
  from a CSS selector with an optional text anchor, an attribute, the redirect
  `Location`, or a JSON Pointer.
- `destructive`: required on writes.

Unchanged rules: templates are substitution only; credentials are referenced,
never stored; requests stay on the declared origin; output is mapped and
validated, never passed through; writes declare reconciliation (`marker` plus
a reconcile read, or `none`). The reconcile read may itself be a
`browser.steps` extract, for example finding the marker text on a list or
search page, so UI writes aren't forced to `none`.

**[Proposal]** Rules for `browser.steps` writes:

- **Commit step.** Every write plan marks exactly one step `commit: true`,
  which is validated at load.
  - A failure before that step's action starts is `failed/not_sent`.
  - A failure once that action starts, or a lost answer, is `uncertain`.
  - A declared `refused_when` extract matching after it is `failed/sent`.
- **Drift guard.** Steps may declare `expect` assertions (an element is
  present, or anchor text equals a value).
  - A mismatch before the commit step is `failed/not_sent` with code
    `source_changed`.
  - The commit step never falls back to a text anchor unless its `expect`
    assertions pass.
  - A `source_changed` result triggers re-qualification.
- **Markers and settle time.**
  - The marker's field and format are a reviewed part of the artifact,
    because on UI apps the marker is visible data.
  - `settle_seconds` must be measured during qualification, not guessed.
  - Prefer a reconcile read that doesn't depend on a search index (for
    example the record list sorted by newest).

An invalid artifact is refused with every problem listed.

## Building an integration

1. **Describe:** goal text, origin, allowed effects, and the credential
   binding.
2. **Sign in** in the headed capability browser. A human step (for example an
   emailed passcode) is allowed and recorded as such.
3. **Demonstrate** the task once. Writes are performed only if authorized.
4. **Plan:** generalize the recording into candidate operations on the lowest
   qualified rung.
5. **Review** names, schemas, kind, effect, `destructive` and reconciliation.
6. **Qualify** live within authority, recording evidence for each operation.
7. **Publish** the artifact with its content hash and provenance.
8. **Serve:** the runtime loads the artifact and registers the capability.

## Executing an integration

1. The MCP request carries an OAuth access token (see
   [Consumer authorization](#consumer-authorization)).
2. Scope check for the integration and operation. A denial means nothing is
   sent.
3. Input schema validation.
4. A write requires an `idempotency_key`. A known key returns its receipt and
   is never re-dispatched.
5. A write's receipt is journaled as `dispatching` and flushed to disk.
6. The runtime runs `http.*` in-process or calls the capability.
7. The outcome is `succeeded`, `failed` (definitive refusal or not sent) or
   `uncertain`.
8. Output is mapped, validated and returned.

## MCP and OpenAPI interfaces

- The official MCP Go SDK, pinned. No new protocol.
- Streamable HTTP and stdio. Streamable HTTP is exercised by a client that
  doesn't use the SDK.
- Tool names are `<integration>__<operation>` (double underscore), because
  many model clients reject `.` in tool names. Control tools are
  `daraja__receipt` and `daraja__reconcile`.
- Annotations: reads are `readOnlyHint: true`. Writes are
  `readOnlyHint: false`, with `destructiveHint` from the reviewed artifact.
- A write's input schema adds a required `idempotency_key`.
- A principal sees only the tools in its scope. An MCP session is bound to the
  principal that created it.
- Error codes: `scope_denied`, `invalid_input`, `session_unavailable`,
  `session_needs_human`, `capability_unavailable`, `capability_busy`,
  `source_changed`, `source_refused`, `outcome_uncertain`,
  `duplicate_in_progress`, `invalid_output`, `not_sent`, `not_reconcilable`.
- OpenAPI is phase 2.

### Consumer authorization

**[Decision D-15]** Consumers authenticate with OAuth 2.1 and identify
themselves with a Client ID Metadata Document, as the MCP authorization
specification describes.

**[Proposal]** The mechanism:

- The runtime is the protected resource. It serves
  `/.well-known/oauth-protected-resource` and answers an unauthenticated call
  with `401` and a `WWW-Authenticate` header that points to that metadata.
- In v1 the runtime embeds a minimal authorization server. It serves
  `/.well-known/oauth-authorization-server` with
  `client_id_metadata_document_supported: true`, the authorization code grant
  with PKCE `S256` only, and `token_endpoint_auth_methods_supported: ["none"]`.
  It has no dynamic client registration.
- **Client identification.** A `client_id` must be a non-root `https` URL. The
  server fetches the document there, without following redirects, with a size
  limit and a timeout. The document's `client_id` must equal the URL, and the
  request's `redirect_uri` must match one of its `redirect_uris` exactly.
  Loopback `redirect_uris` are allowed, as the OAuth native-app guidance
  allows. The fetch reaches only public addresses unless the operator
  configures otherwise, to prevent SSRF.
- **Consent is a grant.** The owner signs in to the consent page with an
  owner passphrase (stored hashed) and chooses which integrations and
  operations, and which credential binding, the client may use. The grant is
  recorded as a principal keyed by `client_id`. A scope that the client
  requests never grants more than the owner chose.
- **Tokens.** Access tokens are opaque, short-lived (default 1 hour), stored as
  sha256, and audience-bound to the runtime's resource URL. Refresh tokens
  rotate on use. Revoking the grant revokes both.
- Static bearer tokens remain only for the stdio transport and for
  operator-run tests. They are off by default on Streamable HTTP.
- Using OAuth here isn't an API key for the source application: it authorizes
  the agent to Daraja, and the application still sees only the user's own
  session (D-8). It describes Daraja's own invoke facade, only for
  operations with a truthful HTTP contract.

## Nanos deployment assumptions

**[Decision D-5]** Nanos boot evidence comes from a Spark job on the DGX
(aarch64) with `ops` and qemu, using KVM if the pod exposes it and software
emulation otherwise. The report names the mode.

**[Proposal]** To prove, not assume:

- A static `CGO_ENABLED=0` linux/arm64 binary. No subprocess, shell or
  browser.
- The image holds the binary, artifacts and CA certificates. Credentials live
  on a separate volume, because ops writes config `Env` into the image. The
  journal lives on a persistent volume.
- Outbound HTTPS to declared origins and to registered capabilities, plus an
  inbound MCP port. Each is exercised.
- Image size, cold start (launch to the first successful MCP `initialize`) and
  call latency are measured with the exact environment.

## Credentials, sessions and authorization

**[Decision D-8]** Daraja uses the application's own user login, not vendor
API keys.

**[Decision D-9, revisited by evidence]** Session acquisition is pluggable,
and HTTP sign-in is tried first. **[Evidence]** HTTP sign-in can't pass the
first target's browser challenge.

**[Proposal]** Session providers, in order of preference:

1. `browser`: the default. The session lives in the capability's persistent
   browser profile and never leaves it. The capability executes.
2. `http_form`: the runtime signs in over HTTP. Only for origins where it
   qualified.
3. `browser_lease`: opt-in, per origin, an owner decision for each
   integration. A host browser leases cookies to the runtime. It's allowed
   only if qualification shows that leased cookies work from the runtime's
   egress. Known risks:
   - A lease is a bearer copy of the user's whole session, not of one
     operation.
   - The TTL is client-side, and the source session outlives it.
   - Challenge clearance may be bound to IP, user agent or TLS fingerprint.
   - Replay may count as a new browser and trigger verification again.

Other rules:

- Principals carry explicit grants for integration, operations, credential
  binding and capability.
- Logs and journals hold no credentials, cookies, CSRF tokens or bodies.
- **[Decision D-6]** The owner's Zendesk test account is a test target with full
  create, edit and delete authorization. It qualifies the generic bridge. It
  isn't the product goal.
- Pages, responses and tool results are untrusted data.

## Uncertainty and recovery

- An uncertain write isn't a failed one. It is never re-dispatched
  automatically. A retry with the same key returns the receipt.
- `daraja__reconcile` runs the declared reconcile read, which may be HTTP or
  `browser.steps`. It finds the effect (`succeeded`) or confirms it's absent
  after the settle window (`not_applied`). If the read fails, the receipt
  stays `uncertain`. **Known gap (issue #5):** "absent" means the read answered
  with a success status and without the `found` value, so an error sent with a
  success status that no `refused_when` entry declares reads as absent.
- On restart, `dispatching` receipts become `uncertain`.
- A lost capability answer is `uncertain`.
- A redirected write, or a success status without the declared response
  shape, is `uncertain`. Operations never follow redirects.
- A `not_sent` failure is final for its key, and a new key is safe.

## Relationship to EAAP

**[Decision D-13]** Daraja is the successor to EAAP. Daraja owns discovery,
artifact, qualification, runtime and capability brokering. EAAP is not a peer
boundary and gets no new work from this RFC.

**[Proposal]** The EAAP row in `portfolio-boundaries.md` should be replaced by
a Daraja row, for example: "Daraja — `zati-run/daraja`: bridges applications
without a usable official API to MCP consumers through their internal,
session-authenticated APIs; owns discovery, artifacts, qualification, the
receipt journal and capability brokering; supersedes EAAP. Clients decide
whether to call a tool; the source application remains the authority." That
file lives outside this repository, so the owner records the row (U-4).

## Testable end-to-end acceptance

**[Proposal]**

| ID | Check |
|---|---|
| A1 | An invalid artifact is refused with reasons, including a write without a `destructive` value or without reconciliation. |
| A2 | A demonstration recorded in a headed browser yields redacted observations (UI steps plus traffic) and a coverage report. No value or salt is persisted. Demonstrations run only under write authorization or a confirmed navigation allowlist. |
| A3 | The planner generalizes the demonstration into candidates. A reviewed integration is published with provenance and a hash. |
| A4 | The runtime serves over Streamable HTTP. A non-SDK client lists tools and calls a read. |
| A4a | An MCP client that identifies itself only by a CIMD URL completes OAuth (PKCE) after the owner consents, and sees exactly the granted tools. An unknown `redirect_uri`, a mismatched `client_id` or a missing PKCE verifier is refused. |
| A5 | A read on rung 1 runs **inside the page** through the browser capability against an internal, session-authenticated endpoint, and returns schema-valid output. |
| A6 | Scope denial sends nothing. |
| A7 | Invalid input sends nothing. |
| A8 | A write runs once and records `succeeded` with an identifier. |
| A9 | A repeated key doesn't dispatch again. |
| A10 | A lost response, from HTTP or from a capability stream that drops mid-call, is `uncertain` and is reconciled from the source. A `browser.steps` failure before the commit step is `not_sent`. |
| A11 | A crash mid-write comes back `uncertain` and is reconciled, not re-sent. |
| A12 | Boot on Nanos and serve a real MCP request from that exact image. Image size, cold start and latency are recorded. |
| A13 | The runtime on Nanos brokers an in-page `http.json` operation to the host capability on the Mac over an authenticated outbound stream. |
| A14 | Zendesk's internal API, as its agent UI uses it, is qualified for one read and one write through the generic bridge, using only the browser session (no official-API credentials). The measured settle time is reported. |
| A15 | A changed source (an internal endpoint that moves or changes shape) yields `source_changed` or `invalid_output`, and no write is dispatched on a drift detected before the request. |
| A16 | An expired session that needs a human step yields `session_needs_human` without blocking. |

## Phased scope

**[Proposal]** Under D-11, all of phase 1 lands in one pull request.

- **Phase 0:** this RFC and the owner decisions.
- **Phase 1a (core claim):**
  - the capability contract (outbound stream, `op_ref` with a hash, legal
    outcomes) and a headed browser capability on the Mac;
  - an in-page `http.json` executor;
  - the demonstration recorder for internal API traffic and the generalizing
    planner (effect proposed, never inferred from the method alone);
  - OAuth 2.1 with CIMD for consumers;
  - the existing runtime, journal and MCP;
  - qualification on a synthetic app whose only API is internal and
    session-authenticated.
- **Phase 1b:** Zendesk's internal API, one read and one write.
- **Phase 1c:** Nanos as scheduler and broker (A12, A13).
- **Phase 2:** `http.form_html` and `browser.steps` (with commit step and
  drift guard), both implemented; the model-backed planner adapter, an OpenAPI facade, signed
  list cursors, Ferro as a capability candidate, `browser_lease` where
  qualified.
- **Phase 3:** a `desktop.native` capability (for example Comuse), with its
  own qualification plan.
- **Phase 4:** hosted multi-tenant registry with per-user custody.

## Owner decisions

| ID | Date | Decision |
|---|---|---|
| D-1 | 2026-10-31 | Scope and non-goals: no CRM/workflow/Zati adapter/IdP; no app-specific code in the core. |
| D-2 | 2026-10-31 | Declarative artifacts interpreted by generic executors. |
| D-3 | 2026-10-31 | Pluggable planner, deterministic first, no hidden model calls. |
| D-4 | 2026-10-31 | First explorer uses a local Chrome on the host. ("Headless" is superseded by evidence; see Builder.) |
| D-5 | 2026-10-31 | Nanos boot on the DGX through Spark, ops and qemu, KVM if possible. |
| D-6 | 2026-10-31 | The owner's Zendesk test account is a full-authority test account. |
| D-7 | 2026-10-31 | Credentials in the repo's gitignored `.env`. Real-target artifacts and evidence are gitignored. |
| D-8 | 2026-10-31 | The application's own user login, not API keys. |
| D-9 | 2026-10-31 | Pluggable sessions, HTTP sign-in first, browser fallback. (Revisited by evidence; see Credentials.) |
| D-10 | 2026-10-31 | Pomerado ideas only; Go only. |
| D-11 | 2026-10-31 | RFC and first slice land in one pull request. |
| D-12 | 2026-10-31 | The goal is a generic bridge to apps without APIs, not a Zendesk connector. |
| D-13 | 2026-10-31 | Daraja supersedes EAAP: "Daraja is the future. It is what EAAP was supposed to be and failed." |
| D-14 | 2026-10-31 | The target is the internal API an app's web client uses (for Zendesk, not its official API), made usable by agents. |
| D-15 | 2026-10-31 | Agents connect over Streamable HTTP MCP, authorized with OAuth and Client ID Metadata Documents. |
| D-16 | 2026-10-31 | The headed browser capability runs on the owner's Mac. |
| D-11 (reaffirmed) | 2026-10-31 | One pull request at the end. |

## Unresolved choices

| ID | Question |
|---|---|
| U-1 | **Closed by evidence:** HTTP sign-in can't pass the first target's challenge. |
| U-2 | How credentials reach a production runtime (volume, metadata or a broker). |
| U-3 | Artifact signing between builder and runtime. |
| U-4 | Direction decided by D-13. The owner still needs to record the Daraja row in `portfolio-boundaries.md` (proposed text in [Relationship to EAAP](#relationship-to-eaap)). That file is outside this repository. |
| U-5 | Per-user credential binding in a hosted deployment. |
| U-6 | Model, if any, for the phase 2 planner. |
| U-7 | **Closed by D-13.** |
| U-8 | **Closed by D-16.** |
| U-9 | **[Proposal]** `browser_lease` stays in the code as opt-in, with qualification deferred to phase 2. Not blocking. |
| U-10 | **Closed by D-14.** |
| U-11 | **Closed:** D-11 reaffirmed. |
| U-12 | How the owner signs in to the consent page in a hosted deployment. v1 uses an owner passphrase on a single-owner runtime, with a global failure lockout. The review recommends a WebAuthn (passkey) owner session, because a reusable passphrase typed into a web form can be phished. |

## Evidence status

| Item | Status |
|---|---|
| Zendesk API password Basic auth | **[Evidence]** 2026-10-31, Mac, curl: `GET /api/v2/users/me.json` with the username/password returned 200 as the anonymous user. |
| Zendesk sign-in over HTTP | **[Evidence]** 2026-10-31, Mac, curl: `/access/login` returned 403, a Cloudflare challenge page, with or without a browser user agent. |
| Zendesk sign-in, headless Chrome | **[Evidence]** 2026-10-31, chromedp v0.21.0, Chrome on macOS: stuck on the challenge (Turnstile) until the 150 s timeout. |
| Zendesk sign-in, headed Chrome | **[Evidence]** 2026-10-31: the challenge passed, the password was accepted, then the page went to two-step verification (passcode emailed for this new browser). Sign-in wasn't completed; it needs the owner to enter the passcode. |
| Zendesk API token deprecation | Owner-reported, not independently verified. |
| Leased cookies from another egress | Not tested. The peer review predicts a challenge (403). |
| Zendesk sign-in through the capability recipe | **[Evidence]** 2026-11-01: `daraja signin` in headed Chrome with a dedicated profile passed the challenge and the password. The owner entered the emailed passcode at `/auth/v3/tsv`, and the profile kept the session for all later runs (explore, record, capability). The first passcode expired while waiting, and a restarted sign-in sent a new one. The login page is `/auth/v3/signin`, not `/access/login`. |
| Source implementation at Revision 6 (historical; since committed, and later revisions add to it) | Phase 1a as revised: the capability contract (outbound long-poll, `op_ref` with artifact hash and version, legal outcomes, broker withdraw/lost semantics), a headed-browser capability host with an in-page `fetch` executor and non-blocking recovery, the `browser` provider, required `destructive`, `reviewed_safe_method` reads, OAuth 2.1 with CIMD (consent as grant, PKCE S256, rotating refresh, persisted hashes, SSRF-guarded metadata fetch), condition-based explorer settling and demonstrate steps (`daraja record`), and `daraja capability`. Not implemented: `http.form_html`, `browser.steps`, salted value correlation in the recorder, an OAuth grant management UI. |
| Synthetic tests | **[Evidence]** 2026-11-01, macOS arm64, Go 1.27.3, `go test -race ./...`: 52 top-level tests and 65 subtests pass, none fail (counted 2026-11-01 after Revision 6, its R1, R2 and R2b fixes, and the merge-review fixes). On 2026-11-03, with `browser.steps` added, the same command ran 114 top-level tests and 161 subtests: all pass, none skipped. The list below is the 2026-11-01 coverage; Revisions 7 to 9 describe what they add. They cover:<br>• OAuth: an SDK OAuth client that identifies itself only by a CIMD URL; receipt scope surviving re-consent; refresh-token reuse revoking the grant; consent lockout across requests; public-address filtering.<br>• Capability: read, write and duplicate; re-sign-in after the source session expires; `session_needs_human` in under 15 s; `capability_unavailable`.<br>• Writes: a lost capability answer is `uncertain`, a retry doesn't re-dispatch, and reconcile finds the effect. A redirected write is `uncertain` on both the in-process and the in-page paths. An idempotency key reused with different input is a conflict.<br>• Other: runtime URL checks and pinning; 8 MiB outcome delivery; the pending human-step guard (real Chrome); synthetic-only credentials without a mode check; GraphQL read-only classification (26 cases, including a `Query`-cased key hiding a mutation, repeated keys and trailing data); a GraphQL GET with a repeated or case-variant `query` parameter blocked; a GraphQL POST with a URL query string blocked; a method-override header or `_method` parameter blocked; inline literals redacted from recorded query documents; a GraphQL query let through and a GraphQL mutation blocked in a read-only walk; a second tab's read recorded and its write blocked; a read-only walk blocking a form posted to a new window, a new window that writes at once, a worker, a beacon, a GraphQL mutation over GET, a hash-only persisted query over GET, an `<a ping>`, a service-worker fetch, and a WebSocket from a page, a dedicated worker, a nested worker, a shared worker and a cross-site frame, each with a writes-allowed control.<br>Each review regression test was confirmed to fail on the earlier code.<br>**Flake, found and fixed:** the reviewer saw `builder` fail in 1 of 7 full runs, and it was reproduced here (1 of 8 stressed runs, plus 1 of 3 concurrent full runs). The explorer lost an observation, here `GET /api/notes/1`, so no `get_note` was planned. Cause: the sent, response and finished network events arrive on separate subscriptions handled by separate goroutines. A request whose "finished" event was handled before its "sent" or "response" event was dropped or shaped without a status. The recorder now merges the three events per request and records it only when all three are in. After the fix: 12 stressed runs of `builder` (48 test runs) and 3 concurrent full runs pass.<br>**Journal:** a torn final line is now truncated at open, so the next append can't fuse onto it. The regression test lost the next receipt without the fix. |
| Nanos boot (A12) | **[Evidence]** 2026-10-31, Spark pod on the DGX (Linux 6.17 aarch64), ops 0.1.47 (latest release), Nanos 0.1.56 arm64 (after `ops update --arm`; the release kernel was missing), qemu 7.2 with **KVM**: image 64 MiB (14 MiB allocated), runtime ELF 11.3 MB, build about 19 s, cold start to MCP `initialize` 470–590 ms. From that image: tools/list, five HTTPS reads to the fixture (4–10 ms warm, 135 ms first), `invalid_input`, scope denial, one write and a non-dispatching duplicate. |
| Nanos volumes | **[Evidence]** ops 0.1.47 can't attach volumes on arm64: it adds `scsi-hd,bus=scsi0.0` but creates no virtio-scsi controller on the `virt` machine (`qemu/qemu_unix.go`). The run therefore uses a recorded fallback that puts the synthetic credentials and journal in the image, with `-unikernel-file-modes` because Nanos reports every file as mode 0777. Since revision 5, that mode refuses any credential binding not marked `synthetic`, so real credentials can't take this path. Pod images are deleted with the pod. |
| Nanos crash recovery (A11 on Nanos) | **[Evidence] Passes after a fix (2026-11-01).** The first run failed: after qemu was killed mid-write and rebooted, the runtime loaded no receipts, so a retry re-sent the write and duplicated it in the fixture. A bisect (`deploy/nanos/durability-bisect.sh`) proved the cause wasn't flushing. The journal was in the image file after the kill, both `dispatching` and `succeeded` lines. The same image file was reused (sha256 unchanged, no rebuild), and the qemu drive had no `-snapshot` or cache option. The journal replayed through its `O_APPEND` handle, which on Nanos doesn't start reading at offset 0. It now replays through a separate read-only handle. Rerun on the same pod setup (KVM, ops 0.1.47, Nanos 0.1.56): with an immediate kill and a 10 s delayed kill, the receipt is found after reboot. In `nanos-run.sh`, with the VM killed during a write: the earlier receipt survives, `recovered_uncertain=1`, a retry with the same key isn't sent, reconcile finds the effect (`succeeded`), and `posts_final=2` (no duplicate). This is the in-image journal fallback, not a volume; a durable volume on arm64 remains unqualified. |
| Nanos brokering to the Mac capability (A13) | **[Evidence]** 2026-10-31: the runtime on Nanos (DGX pod, published port 18080) accepted the Mac capability's outbound registration. A non-SDK MCP client on the Mac called it: three in-page reads ran in the Mac's Chrome profile against a loopback synthetic app, and a brokered write ran once with its duplicate answered from the receipt (one POST). The capability ran headless for this synthetic origin. |
| Real-target qualification (A14) | **[Evidence] Passes (2026-11-01).** The owner's Zendesk test account, reached only through the browser session: no official-API credentials and no API token.<br>**Discovery.** A read-only walk (with GraphQL queries allowed) and an owner demonstration recorded in every tab (164 observations). The agent UI reads through REST (`/api/v2/tickets/{id}`, `/api/lotus/tickets/{id}/conversations.json`, `/api/lotus/tickets/recent`, `/api/v2/search.json`). It writes through GraphQL mutations (`CreateIssueTicketMutation`, `UpdateTicketMutation`), which need the `apollographql-client-name`/`-version` headers. Introspection is disabled. The comment format enum (`PLAIN`) and the required `uploads` field came from validation errors, sent with `uploads` left out so no probe executed.<br>**Artifact** `zendesk-agent@1` (private, `private/zendesk/integrations/`): 5 reads and 2 writes. Both writes declare `marker` reconcile, as the deferred two-phase claim requires. `create_ticket` puts the marker in a ticket tag and reconciles through `find_ticket_by_tag`. `add_internal_note` (`isPublic:false`) puts it in the note text and reconciles through `find_ticket_by_text`. Each write's identifier is `/data/<mutation>/ticket/id`, and `settle_seconds` is 180.<br>**Run.** Runtime on the Mac (loopback, static operator token, not OAuth), the Mac capability in headed Chrome, and a non-SDK MCP client:<br>• Reads: 175–200 ms (search 400 ms).<br>• `add_internal_note`: 727 ms, and the note exists once, internal.<br>• `create_ticket`: 561 ms, and exactly one ticket was created.<br>• Same-key retries replayed in under 1 ms without sending. Different input on the same key returned `idempotency_key_conflict`.<br>• **Settle:** search found the note marker within 9 s and the tag marker within 6 s (upper bounds).<br>• **Failure path:** a note to a missing ticket got HTTP 200 with a GraphQL failure and no identifier. It was classified `outcome_uncertain`, not `succeeded`. Reconcile kept it uncertain inside the settle window and resolved it `not_applied` after.<br>• Logs: 30 executed jobs, no warnings, and no ticket content or markers.<br>**Not covered:** OAuth/CIMD for this client (D-15; OAuth is qualified only synthetically), a Nanos runtime for Zendesk, a session expiring mid-run, and drift of `apollographql-client-version` across Zendesk deploys. |
| Read-only walk under the socket guards | **[Evidence]** 2026-11-02, same Zendesk account and profile, headed Chrome, the same walk task as A14. The session was still signed in. 131 observations and 431 requests, against 81 and 250 before the guards (the same task, so the runs aren't a controlled comparison). 120 writes were blocked. 3 cross-site frames or workers were held paused and reported as gaps. One blocked route seen earlier didn't recur (a billing experiment's `participation` POST). Of the routes the artifact uses, the walk saw `/api/v2/tickets/{id}`, `/api/v2/search.json` and `/api/graphql`. It didn't see `/api/lotus/tickets/{id}/conversations.json` or `/api/lotus/tickets/recent`. The earlier read-only walk didn't see them either; they came from the owner demonstration, so this walk doesn't show whether the guards affect them. That run predated the fix that stops pages before the guards end, so its held targets were released, and its last page kept running without the write guard, until the browser closed moments later. **Rerun with the fix,** 2026-11-03, same task, with the walk ending by stopping Chrome: 131 observations, 426 requests, 116 writes blocked, 3 targets held. No Chrome process stayed on that profile. Blocked and request counts vary from run to run, because the app chooses what to send. |
| Client version and declared failure (Revision 7) | **[Evidence]** 2026-11-02, the owner's Zendesk test account, Mac runtime and capability in headed Chrome, static token, a non-SDK MCP client, artifact `zendesk-agent@3`.<br>• Client version read at session time: one read (about 300 ms) and one internal note (593 ms, `succeeded`, its retry replayed in under 1 ms without sending).<br>• A note to a missing ticket: `source_refused`, `failed` in 310 ms, against `uncertain` until the 180 s window before. A retry with the same key returned the final receipt without sending.<br>• Logs: no warnings.<br>**Not covered:** `create_ticket` declares the same failure shapes, but no failing create was sent, and `CreateOrUpdateTicketFailure` was never seen in an answer. |
| `browser.steps` on a real application | **[Evidence]** 2026-11-02, the owner's Zendesk test account, Mac runtime and capability in headed Chrome, a non-SDK MCP client, one reviewed read that opens `/agent/tickets/{id}` and waits for the conversation. The existing ticket answered in 2.9 s with its ID from the URL and the conversation text from the page. A missing ticket answered `source_changed` after 16 s (the step's timeout). No log warnings. **Not covered:** writes. |
| Deployed service | None. |
