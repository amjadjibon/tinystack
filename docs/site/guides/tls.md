# HTTPS and the local CA

TinyStack optionally serves the AWS gateway over HTTPS alongside HTTP. TLS is off
by default. Enable it with `tinystack start --tls-address 127.0.0.1:4443`, or YAML:

```yaml
tls:
  enabled: true
  address: 127.0.0.1:4443
  names: ["signin.local.test", "*.custom.local.test"]
storage:
  mode: persistent
  path: ./data
```

The HTTPS listener shares HTTP's gateway, routing, authentication and timeouts.
It requires TLS 1.2 or newer. Port `0` selects an ephemeral port, reported in the
`https ready` log. It does not enable HTTPS for the separate console listener.

## Generate and export

On first HTTPS startup, TinyStack generates an ECDSA P-256 root named
**TinyStack Local CA**, valid for ten years. The CA is reused on later starts.
Each server name a client asks for gets its own leaf on first use, valid for 365
days (never beyond the CA expiry). Leaves and their keys stay in memory.

With persistent storage, the CA lives at `storage.path/tls/ca.pem` and
`storage.path/tls/ca-key.pem`; `~` and `~/` expand like the storage directory.
With memory storage, it lives in the OS user config directory under `tinystack/tls`:
`~/Library/Application Support/tinystack/tls` on macOS,
`$XDG_CONFIG_HOME/tinystack/tls` (or `~/.config/tinystack/tls`) on Linux, and
`%AppData%\tinystack\tls` on Windows. Memory-mode AWS state remains ephemeral,
but the CA persists. Run export and start as the same user with the same config
and environment so they select the same CA.

```sh
tinystack ca export --config tinystack.yaml --out tinystack-ca.pem
# Without --out, only the public certificate PEM is written to stdout:
tinystack ca export --config tinystack.yaml > tinystack-ca.pem
```

Export creates the CA if absent; it does not start a listener. The CA directory
is created with mode 0700, its key with 0600, and its certificate with 0644
(subject to the OS umask and filesystem permissions). Never distribute the key.
Incomplete, corrupt or expired CA files cause an explicit error; TinyStack never
silently replaces a trusted CA. To rotate, stop TinyStack, remove its old trust
entry, move the `tls` directory aside, then export and trust the new certificate.
There is no automatic renewal or ACME.

## Names and custom certificates

TinyStack issues a leaf for the exact name in the client's TLS server name (SNI) when it is `localhost`,
`localhost.localstack.cloud`, or ends in `.amazonaws.com`, `.amazonaws.com.cn`, `.api.aws`, `.on.aws`,
`.amazoncognito.com`, `.localhost.localstack.cloud` or `.localhost`. That covers every Region, S3 virtual host
(dotted bucket names too), API Gateway, Lambda URL and Cognito hosted domain without wildcards. Certificate coverage
does not add support for service APIs or partitions.

Add other names with `tls.names`: exact DNS names, wildcards of one leading label (`*.custom.local.test` covers
`a.custom.local.test`, not `b.a.custom.local.test`), or IP literals. A client that sends no server name, or another
name, gets a leaf for `localhost`, `127.0.0.1`, `::1` and the configured IPs. URLs, ports, empty names and
partial or interior wildcards fail explicitly.

To use an existing server certificate chain and matching private key instead:

```yaml
tls:
  enabled: true
  address: 127.0.0.1:4443
  cert_file: ./server-chain.pem
  key_file: ./server-key.pem
```

Both PEM files are required. `tls.names` cannot be combined with supplied files:
the supplied certificate determines name coverage. No local CA is generated in
this mode, and `ca export` fails explicitly; obtain the CA from your issuer.

## Client trust

Prefer trust scoped to the development process. Use absolute PEM paths:

```sh
export AWS_CA_BUNDLE="$PWD/tinystack-ca.pem"       # AWS CLI / supporting SDKs
export NODE_EXTRA_CA_CERTS="$PWD/tinystack-ca.pem" # Node.js, set before startup
export REQUESTS_CA_BUNDLE="$PWD/tinystack-ca.pem"  # Python Requests
aws --endpoint-url https://127.0.0.1:4443 sts get-caller-identity
```

Support varies by SDK. For Go, configure an `http.Transport` with a
`tls.Config.RootCAs` pool containing the exported PEM, or configure the SDK's
custom CA bundle loader. Keep verification enabled. A bundle that replaces the
client's roots must also contain any public roots needed for other connections.
See [AWS CLI environment variables](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-envvars.html),
[Node.js extra CAs](https://nodejs.org/api/cli.html#node_extra_ca_certsfile), and
[Requests certificate verification](https://requests.readthedocs.io/en/latest/user/advanced/#ssl-cert-verification).

For browsers or system-trust clients, these are **manual, optional** steps.
TinyStack never changes your keychain or trust store:

- **macOS:** import only `tinystack-ca.pem` into Keychain Access's login keychain,
  open **TinyStack Local CA**, expand Trust, and select Always Trust for SSL.
  See [Apple's certificate import guide](https://support.apple.com/guide/keychain-access/kyca2431/mac).
- **Debian/Ubuntu Linux:** copy the exported PEM to
  `/usr/local/share/ca-certificates/tinystack-local-ca.crt`, then run
  `sudo update-ca-certificates`. Other distributions and browsers can use
  separate trust stores; follow their CA import instructions.
- **Windows:** import the certificate into Current User → Trusted Root
  Certification Authorities, or run
  `certutil -user -addstore Root tinystack-ca.pem`.
  See [Microsoft certutil](https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/certutil).

Trust only a CA whose private key you control. Remove its trust entry when you
finish using it.

## DNS and port 443

Combine HTTPS with the [DNS listener](dns.md) for default AWS hostnames. An SDK
reaches `https://<service>.<region>.amazonaws.com` without an endpoint override
only when that name resolves to TinyStack, **port 443 reaches the TLS listener**,
and the client trusts the CA. DNS changes addresses, never ports or schemes.
The default HTTPS port is 4443. Listening on 443 may need privileges; alternatively
arrange a port forward from 443 to 4443 yourself. TinyStack does not configure
port forwarding, DNS settings or system trust. Preserve the AWS Host header.
