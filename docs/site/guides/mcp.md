# Console MCP server

```sh
task build
./bin/tinystack start
claude mcp add --transport http tinystack http://127.0.0.1:4567/mcp
```

The console listener serves a [Model Context Protocol](https://modelcontextprotocol.io) server at `/mcp`, so an AI
assistant can inspect and drive a running TinyStack: check health, list requests, resources and deliveries, inject
faults, load cost fixtures, read init script status and reset state. It uses the Streamable HTTP transport.

Only the console build (`task build`, `go build -tags console`) has it. Headless builds (`task build:headless`) have
no MCP server and do not link the MCP SDK.

## Connect a client

=== "Claude Code"

    ```sh
    claude mcp add --transport http tinystack http://127.0.0.1:4567/mcp
    ```

    Or, for a project, in `.mcp.json`:

    ```json
    {
      "mcpServers": {
        "tinystack": { "type": "http", "url": "http://127.0.0.1:4567/mcp" }
      }
    }
    ```

=== "Cursor"

    In `.cursor/mcp.json`:

    ```json
    {
      "mcpServers": {
        "tinystack": { "url": "http://127.0.0.1:4567/mcp" }
      }
    }
    ```

=== "VS Code"

    In `.vscode/mcp.json`:

    ```json
    {
      "servers": {
        "tinystack": { "type": "http", "url": "http://127.0.0.1:4567/mcp" }
      }
    }
    ```

Use the console's port if `--console-port` changes it.

## Tools

Each tool calls one of TinyStack's own endpoints in-process and returns that endpoint's JSON (the metrics tool returns
the Prometheus text). An answer other than 2xx, or an argument the tool does not name, is a tool error.

| Tool | Endpoint | Arguments |
| --- | --- | --- |
| `health` | `GET /_tinystack/health` | none |
| `reset` | `POST /_tinystack/reset` | `init`: re-run the ready-stage init scripts |
| `list_requests` | `GET /_tinystack/requests` | none |
| `list_resources` | `GET /_tinystack/resources` | none |
| `list_deliveries` | `GET /_tinystack/deliveries` | none |
| `get_metrics` | `GET /_tinystack/metrics` | none |
| `get_faults` | `GET /_tinystack/faults` | none |
| `set_fault` | `PATCH /_tinystack/faults` | one [fault rule](fault-injection.md): `service`, `operation`, `region`, `account`, `error`, `probability`, `latencyMs` |
| `clear_faults` | `DELETE /_tinystack/faults` | none |
| `list_cost_fixtures` | `GET /_tinystack/costs/fixtures` | none |
| `put_cost_fixture` | `PUT /_tinystack/costs/fixtures/<id>` | `id`, `provenance`, `lines` |
| `init_status` | `GET /_tinystack/init[/<stage>]` | `stage`: `boot`, `start`, `ready` or `shutdown` |

## Security

- The endpoint answers only requests whose `Host` is `localhost`, a `*.localhost` name, a loopback IP or the console's
  listen address, so a DNS-rebound name cannot reach it.
- Browsers may call it only from the console's own origin; other origins get `403 Forbidden`. Clients that send no
  `Origin` header, as MCP clients do, are not affected.
- With [console sign-in](sso.md) configured, every endpoint except health needs a signed request, which MCP calls
  do not carry, so those tools fail with `403`.
