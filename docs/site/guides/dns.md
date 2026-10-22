# DNS listener

TinyStack can resolve AWS hostnames locally over UDP and TCP. DNS is **off by
 default**. Enable it in YAML:

```yaml
dns:
  enabled: true
  address: 127.0.0.1:53
  upstreams: ["192.0.2.53:53"] # replace with your reachable resolver
```

Or run `tinystack start --dns-address 127.0.0.1:1053`. The flag enables DNS.
The default address is `127.0.0.1:53`; binding port 53 may require privileges.
Without `upstreams`, resolvers are read from `/etc/resolv.conf`. Configured
upstreams use `host:port` syntax, including brackets for IPv6. Choose upstreams
that do not point back to TinyStack, to avoid forwarding loops.

Names beneath `amazonaws.com`, `amazonaws.com.cn`, `api.aws`, and
`localhost.localstack.cloud`, plus Lambda function URLs beneath `on.aws`, resolve
to `server.host`. This includes ELB, execute-api, AppSync, and virtual-hosted S3
names. An unspecified listen IP (`0.0.0.0` or `::`) advertises the corresponding
loopback IP. A literal `server.host` IP is required when DNS is enabled. IPv4
listeners answer A; IPv6 listeners answer AAAA. The other family has an empty
successful answer, preventing fallback to public AWS addresses.

Route 53 hosted zones in the default account take precedence, using the longest
matching zone. Stored records, including CNAMEs, are returned with their TTLs.
Route 53 is global; `defaults.region` does not restrict its records. Aliases and
routing policies return SERVFAIL; DNS does not simulate their routing decisions.
Other names forward to upstreams, preserving responses including NXDOMAIN and
SERVFAIL. No queries are sent to AWS by TinyStack itself.

## HTTP, ports, and certificates

The AWS gateway has an optional [HTTPS listener with a local CA](tls.md).
DNS changes addresses, **not ports or schemes**. SDKs and CLIs normally use HTTPS
on port 443. Enable TLS, export and trust its CA, and arrange for port 443 to
reach the TLS listener (default 4443) to use default SDK endpoints. Binding 443
may require privileges; port forwarding is a manual alternative.
For local HTTP clients, select HTTP and arrange port 80 forwarding to the
TinyStack HTTP port (or configure `server.port: 80`). Preserve the original Host
header. TinyStack never changes system trust, DNS settings or port forwarding.
Never disable certificate verification as a substitute for a trusted local CA.

## macOS split DNS

Create one `/etc/resolver/<suffix>` file per intercepted suffix, for example
`/etc/resolver/amazonaws.com`:

```text
nameserver 127.0.0.1
port 1053
```

Repeat for `amazonaws.com.cn`, `api.aws`, and `localhost.localstack.cloud`;
add `on.aws` for Lambda URLs and each custom Route 53 zone you want to resolve.
Use `scutil --dns` to inspect resolver selection. These are manual setup steps;
TinyStack never changes system DNS settings.

## Linux systemd-resolved

Use a dedicated local interface or systemd-resolved link configuration with
`DNS=127.0.0.1:1053` and routing-only domains such as
`Domains=~amazonaws.com ~amazonaws.com.cn ~api.aws ~localhost.localstack.cloud`.
Add custom hosted zone suffixes as needed. Apply this through your distribution's
network manager and inspect it with `resolvectl status`. Avoid using TinyStack
as its own upstream if `/etc/resolv.conf` points at systemd-resolved.

## Docker

Bind DNS to an address reachable from the client container, for example
`dns.address: 0.0.0.0:53`, and use `docker run --dns <DNS-server-IP> ...`.
Docker's `--dns` uses port 53. `127.0.0.1` inside a container means that container,
so use a reachable host or TinyStack container address. Set `server.host` to that
reachable IP so DNS answers can reach the gateway. Publish both UDP and TCP DNS
ports when crossing container networks. The HTTP/TLS port requirements above
still apply.
