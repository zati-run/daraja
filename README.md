# Daraja

Daraja turns web applications without a usable API into scoped
[MCP](https://modelcontextprotocol.io) tools. It uses the same internal,
session-authenticated API that the application's own web client calls. A person
demonstrates a task once, Daraja proposes typed operations, and a reviewer
approves them. After that, each tool call runs deterministically, with no LLM in
the path.

*Daraja* is Swahili for "bridge".

> **Status: early.** The design in
> [RFC 001](docs/rfc/001-daraja-architecture.md) is a draft and hasn't been
> accepted, and there is no deployed service. See [Status](#status) for what has
> been shown to work.

## How it works

1. **Build.** On a host with a headed browser, you sign in and walk through a
   task. Daraja records the requests the page makes, keeping their shapes but
   not their values, and plans an integration artifact: named operations with
   JSON Schemas, an effect class, and a way to reconcile writes.
2. **Review.** A person confirms each operation's effect. Daraja never infers
   a write from the HTTP method alone.
3. **Serve.** The runtime publishes the operations as MCP tools over Streamable
   HTTP. Consumers authenticate with OAuth 2.1 and identify themselves with a
   Client ID Metadata Document.
4. **Execute.** The runtime journals a receipt before each write. It runs the
   request itself, or it brokers the request to an execution capability: a
   headed browser that holds the user's session and makes the call from inside
   the page.

A write that may have happened is never silently retried. Retrying with the
same idempotency key returns the original receipt. An unknown outcome stays
`uncertain` until reconciliation finds the effect or rules it out.

The application stays the authority for its data and actions. Holding a browser
session grants nothing: a caller can only run the operations an owner granted
to it, for each integration.

## Quickstart

This runs the runtime against a synthetic notes application on your machine. It
needs Go 1.27.3 or later, and it doesn't need a browser or a real account.

Build the binaries and create a working directory:

```sh
git clone https://github.com/zati-run/daraja && cd daraja
mkdir -p demo/integrations
go build -o demo/ ./cmd/... ./internal/cmd/notesfixture
cd demo
```

Point the sample integration at the local application, and validate it:

```sh
sed 's#http://127.0.0.1:1#http://127.0.0.1:9555#' \
  ../integration/testdata/notes.json > integrations/notes.json
./daraja validate integrations/notes.json
```

Create a caller, its grants, and the application's test credentials:

```sh
./daraja token > token.txt
sed -n 's/^token=//p' token.txt > token
cat > principals.json <<JSON
{"principals": [{
  "id": "agent",
  "token_sha256": "$(sed -n 's/^sha256=//p' token.txt)",
  "grants": [{
    "integration": "synthetic-notes",
    "operations": ["get_note", "find_note_by_ref", "create_note"],
    "credential": "notes"
  }]
}]}
JSON
cat > credentials.json <<JSON
{"bindings": {"notes": {
  "values": {"username": "synthetic-user", "password": "synthetic-pass"},
  "synthetic": true
}}}
JSON
chmod 600 credentials.json
```

Start the application and the runtime. Static tokens are for local testing
only; the consumer interface is OAuth.

```sh
./notesfixture -plain -listen 127.0.0.1:9555 > fixture.log 2>&1 &
./daraja-runtime -integrations integrations -principals principals.json \
  -credentials credentials.json -journal journal.jsonl \
  -listen 127.0.0.1:8080 -allow-static-tokens > runtime.log 2>&1 &
```

Call the tools:

```sh
./daraja-mcpcheck -token-file token -list
./daraja-mcpcheck -token-file token -call synthetic-notes__get_note -args '{"id":1}'
./daraja-mcpcheck -token-file token -call synthetic-notes__create_note \
  -args '{"title":"hello","body":"first","idempotency_key":"demo-1"}'
```

Run the last command again. It returns the same receipt, and the application
sees only one `POST /api/notes`:

```sh
grep -c 'POST /api/notes' fixture.log
```

When you finish, stop the background processes with `kill %1 %2`.

## Commands

| Binary | Command | Purpose |
|---|---|---|
| `daraja` | `signin` | Sign in through a headed browser with a dedicated profile. |
| | `explore` | Run a scripted walk and record traffic. Read-only tasks block writes in the browser. |
| | `record` | Record a walk that includes steps a person demonstrates. |
| | `plan` | Turn observations into a candidate integration artifact. |
| | `validate` | Check integration artifacts. |
| | `capability` | Run the execution capability, which dials out to a runtime. |
| | `lease` | Run a host-side session lease broker. |
| | `token`, `owner-passphrase` | Generate a caller token, or an owner passphrase hash for OAuth consent. |
| `daraja-runtime` | | Serve MCP, keep the receipt journal, and broker calls. |
| `daraja-mcpcheck` | | Call a runtime from a small MCP client, for testing. |

`validate` takes artifact files and `token` takes no arguments. For the other
commands, use `-h` to see their flags.

## Status

| Area | State |
|---|---|
| Execution | `http.json`, in the runtime or inside the page through the browser capability. |
| Real application | Qualified against a Zendesk test account through its agent UI's internal API: reads, one ticket created, and one internal note added. The runtime ran locally with a static token. |
| Nanos | Boot, crash recovery of the journal, and brokering to a capability on another host pass on a synthetic application. |
| OAuth 2.1 with CIMD | Covered by tests with a synthetic client. Not yet qualified with the Zendesk run. |
| `http.form_html`, `browser.steps` | Designed, not implemented. |
| Deployed service | None. |
| CI | `gofmt`, `go vet` and the full race test suite, on every pull request and on `main`. |

The [evidence table in the RFC](docs/rfc/001-daraja-architecture.md#evidence-status)
records the environment and method for each result, and known limits such as
the socket exposure in read-only walks.

Daraja doesn't claim compatibility with arbitrary applications. Internal APIs
change without notice, so an integration needs requalifying when its application
changes. Use Daraja only with accounts and applications you are authorized to
automate, and check that doing so is allowed by the application's terms.

## Development

```sh
go vet ./...
go test -race ./...        # Browser tests need Chrome, and skip if it's missing.
go test -race -short ./... # Skips the tests that start Chrome.
```

CI runs `gofmt`, `go vet` and the full race test suite on every pull request
and on `main`, and it fails if any test skips.

## License

[Apache 2.0](LICENSE)
