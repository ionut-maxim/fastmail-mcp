# fastmail-mcp

An [MCP](https://modelcontextprotocol.io) server for
[Fastmail Masked Email](https://www.fastmail.com/features/masked-email/). It
lets Claude (or any MCP client) find, create, enable, disable and edit your
masked email addresses. It cannot delete them.

Built on the [`fastmail`](https://github.com/ionut-maxim/fastmail) Go client
and the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).

## Tools

| Tool | What it does |
|---|---|
| `list_masked_emails` | Lists addresses, newest first. Filters: `states` (`enabled`, `disabled`, `pending`, `deleted`; default enabled and disabled), `domain` (a site, matches its subdomains too), `query` (text in the description, address or site) and `limit` (default 50, at most 500). Read-only. |
| `create_masked_email` | Creates an address for a site: `forDomain` and `description` are required, `url` and `emailPrefix` optional. Always created **enabled** (a `pending` address is removed by Fastmail after 24 hours). Returns the new address. |
| `enable_masked_email` | Enables an address, by `id` or `email`. Refuses addresses in the `deleted` state. |
| `disable_masked_email` | Disables an address, by `id` or `email`: its mail goes to the trash. Undo with `enable_masked_email`. Refuses addresses in the `deleted` state. |
| `update_masked_email` | Changes `description`, `url` or `forDomain`, by `id` or `email`. |

Every tool returns compact records: `id`, `email`, `state`, `forDomain`,
`description`, `url`, `createdAt`, `lastMessageAt`.

`forDomain` takes `example.com`, `https://example.com` or a full URL and is
stored as the site's origin (`https://example.com`), the form Fastmail uses.
It must be http or https with a host and, if given, a valid port. `url` must
be an http or https link. `description` is at most 256 characters; `url` and
`forDomain` at most 2048.

## Install

**Binary**: download an archive for Linux or macOS (amd64 or arm64) from the
[releases](https://github.com/ionut-maxim/fastmail-mcp/releases), check it
against `checksums.txt`, and put `fastmail-mcp` on your `PATH`.

**Go**:

```sh
go install github.com/ionut-maxim/fastmail-mcp/cmd/fastmail-mcp@latest
```

**Container** (linux/amd64 and linux/arm64, distroless, runs as nonroot):

```sh
docker run --rm -p 8080:8080 \
  -e FASTMAIL_TOKEN -e MCP_AUTH_TOKEN \
  ghcr.io/ionut-maxim/fastmail-mcp:latest
```

The image serves HTTP on port 8080 by default. To use it over stdio instead,
pass an empty address: `docker run -i --rm -e FASTMAIL_TOKEN ghcr.io/ionut-maxim/fastmail-mcp:latest -http=`.

## Configuration

| Setting | |
|---|---|
| `FASTMAIL_TOKEN` (env, required) | Fastmail API token. Create it under **Settings → Privacy & Security → Manage API tokens** and give it **only** the Masked Email scope. |
| `MCP_AUTH_TOKEN` (env, required with `-http`) | Bearer token clients must send over HTTP. Use a long random value, such as `openssl rand -hex 32`. The server refuses to start in HTTP mode without it. |
| `-http ADDR` (flag) | Serve streamable HTTP on `ADDR` (such as `:8080`) at `/mcp` instead of stdio. |
| `-version` (flag) | Print the version and exit. |

Over HTTP:

- `POST/GET/DELETE /mcp` needs `Authorization: Bearer <MCP_AUTH_TOKEN>`;
  anything else gets `401`.
- `GET /healthz` is open and returns `200 ok` without calling Fastmail, for
  liveness and readiness probes.
- The server is stateless, so it can run behind a load balancer with any
  number of replicas.
- It shuts down gracefully on `SIGTERM`.
- If a client disconnects mid-call, its Fastmail request is cancelled
  (for clients on MCP protocol 2026-07-28 or later).
- The MCP SDK's DNS-rebinding protection is on: a request that arrives on a
  loopback address (`127.0.0.1`, `::1`) with a non-localhost `Host` header is
  refused with `403`. Reach the server through a Kubernetes Service or an
  ingress, not through a sidecar proxy that forwards to `127.0.0.1` with the
  public `Host`.

Logs are JSON on stderr. Each tool call is logged with its name, whether it
failed and how long it took; arguments, results and tokens are never logged.
Fastmail errors without a specific meaning reach the client as a generic
"Fastmail request failed" and are logged at warn level with the detail.

## Claude Code

Local, over stdio:

```sh
claude mcp add fastmail-masked-email \
  -e FASTMAIL_TOKEN=your-fastmail-token \
  -- fastmail-mcp
```

Remote, over HTTP with the bearer token:

```sh
claude mcp add --transport http fastmail-masked-email \
  https://mcp.example.com/mcp \
  --header "Authorization: Bearer $MCP_AUTH_TOKEN"
```

Or in a project's `.mcp.json`, reading the token from your environment:

```json
{
  "mcpServers": {
    "fastmail-masked-email": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer ${MCP_AUTH_TOKEN}" }
    }
  }
}
```

## Security

- **No delete.** There is no delete tool and no tool can set an address to
  `deleted` (or back to `pending`): `disable_masked_email` is the way to stop
  an address, and it can be undone. Addresses already `deleted` are left
  alone: enable and disable refuse them. Deleting has to be done in Fastmail. The
  code enforces this twice: the server's store interface has no `Delete`, and
  every update goes through a guard that only lets `enabled` and `disabled`
  through.
- **Scope the Fastmail token to Masked Email only.** The server needs nothing
  else, and a narrow token limits what a leak exposes.
- Anyone holding `MCP_AUTH_TOKEN` can create and change addresses on the
  account. Serve HTTP only behind TLS, keep the token in a secret store, and
  rotate it if it leaks. The token is compared in constant time.
- Neither token is ever logged.

## Development

Tools are managed with [mise](https://mise.jdx.dev):

```sh
mise run check             # gofmt check, go vet, go test
mise run build             # bin/fastmail-mcp
mise run run               # run over stdio (needs FASTMAIL_TOKEN)
mise run release:check     # validate .goreleaser.yaml
mise run release:snapshot  # build archives and images into dist/, no publishing
```

`go test ./...` never touches the network: the tools run against an
in-process fake JMAP server.

## Contributing

Commits follow [Conventional Commits](https://www.conventionalcommits.org)
(`feat:`, `fix:`, `docs:`, `chore:`, `ci:`, `test:`, `refactor:`; `!` for a
breaking change). [release-please](https://github.com/googleapis/release-please)
turns them into a release PR; merging it tags the release, and
[goreleaser](https://goreleaser.com) attaches the binaries and pushes the image.

## License

[MIT](LICENSE)
