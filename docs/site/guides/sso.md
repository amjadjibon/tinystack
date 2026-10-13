# Console single sign-on

People can sign in to the TinyStack console with an external OpenID Connect (OIDC) provider. Their provider groups
choose an account and an IAM role, and TinyStack issues temporary STS credentials for that role. IAM
`identity.authorization` (`soft` or `enforce`) and the request inspector then apply to each person. One generic
implementation works with Authelia, Zitadel, Keycloak, Authentik, Okta and Microsoft Entra ID; nothing in TinyStack is
specific to a provider. SAML and SCIM are not part of this feature; the AWS CLI signs in with `tinystack login`.

Single sign-on is off unless `console.auth.oidc` is configured. Without it every behavior is unchanged.

## Configuration

```yaml
identity:
  mode: verify            # required: SSO sessions are signed credentials
  authorization: enforce  # soft or enforce, or SSO identifies people without limiting them
  secret_keys: { root: secret }
console:
  auth:
    oidc:
      issuer: https://idp.example.com/realms/tinystack
      client_id: tinystack-console
      client_secret_file: /run/secrets/oidc     # a file; never an inline secret. Optional for a public PKCE client
      redirect_url: https://console.example.com/_tinystack/auth/callback
      claim: groups                             # claim with the group names: a string or a list
      scopes: [profile, email, groups]          # besides openid; default is profile and email
      session_seconds: 3600                     # 900-43200, default 3600
      roles:                                    # first group a person has, in this order, wins
        platform-admins: { role: "arn:aws:iam::000000000000:role/Admin", admin: true }
        team-b-dev:      { role: "arn:aws:iam::111111111111:role/Developer" }
```

| Setting | Rules |
| --- | --- |
| `issuer` | `https`, or `http` for `localhost` and loopback addresses only; no query, fragment or credentials. It must equal the provider's `issuer` exactly. |
| `redirect_url` | Same scheme rule; the path must be `/_tinystack/auth/callback`. Register it at the provider as the client's redirect URI. |
| `client_secret_file` | A file read at startup (surrounding whitespace removed). Startup fails when it is unreadable or empty. |
| `claim` | A claim name; its value is a string, a list of strings, or an object whose keys are the groups. A dotted name such as `realm_access.roles` descends through nested objects; a claim whose own name contains dots, such as `https://example.com/roles`, is tried whole first. |
| `session_seconds` | The lifetime of the issued credentials. It must not exceed the role's `MaxSessionDuration` (3600 by default; set it on the role for longer sessions). |
| `roles` | Group name (exact, case sensitive) to role ARN. The account is the one in the ARN. A role ARN is either an administrator mapping (`admin: true`) or not, never both. At least one entry. |

Startup fails when `identity.mode` is not `verify`, when `server.console_port` is `0`, or when any of these checks
fail. With `identity.authorization: off`, TinyStack logs a warning: people are identified but not restricted.

The roles must exist in IAM (`CreateRole` and a permissions policy); TinyStack does not evaluate their trust policy,
because the group mapping is the trust. A mapped role that does not exist gives the person an explanatory page, and
nothing is issued.

## What happens when someone signs in

1. **Sign in with SSO** (the console session dialog) opens `/_tinystack/auth/login`. TinyStack creates a login
   transaction (state, nonce, PKCE verifier; ten minutes) and redirects to the provider with a PKCE `S256` challenge.
2. The provider authenticates the person and redirects to `/_tinystack/auth/callback`. TinyStack checks the state,
   exchanges the code with the PKCE verifier, and verifies the ID token with the provider's published keys (signature,
   issuer, audience and expiry; asymmetric algorithms only) and the nonce.
3. The groups in `claim` select the first matching role. No match: a 403 page, nothing issued. Otherwise STS issues a
   role session named after `email`, else `preferred_username`, else `sub`.
4. TinyStack keeps the session in memory under an HttpOnly `SameSite=Strict` cookie that lasts until the credentials
   expire. After every page load the console collects the credentials with a same-origin
   `POST /_tinystack/auth/session`, so a reload or a new tab stays signed in. It keeps them only in the page's memory
   (never in storage, readable cookies or URLs) and signs every console action with them. About a minute before they
   expire it signs in again, which the provider usually completes without asking.
5. **Sign out** ends the session, forgets the credentials in the tab and offers the provider's end-session link when
   the provider advertises one. The issued credentials cannot be revoked, as on AWS; they expire at `session_seconds`.
   Sessions live in TinyStack's memory, so restarting TinyStack signs everyone out.

The console shows who is signed in, the role and account, whether they are an administrator, and when the session
ends. Console actions of a signed-in person are evaluated by IAM like AWS API calls: with `enforce` a denied action
returns an explained `AccessDeniedException`, and with `soft` it runs and the decision is recorded. The request
inspector shows each person's session (`assumed-role/<Role>/<email>`).

### Administrative endpoints

When OIDC is configured, `/_tinystack/reset`, `faults`, `requests`, `deliveries`, `init`, `/_localstack/chaos/*`
and `/_localstack/init` (on both listeners) require a request signed with SigV4 for the service name `tinystack` by
account root credentials (a key from `identity.secret_keys`, or a root session) or by a person whose role is marked
`admin: true`. Everyone else gets 403. The console signs the request inspector this way for you. For scripts:

```sh
curl --aws-sigv4 "aws:amz:us-east-1:tinystack" --user "$KEY:$SECRET" \
  -X POST http://127.0.0.1:4567/_tinystack/reset
```

`resources` and `metrics` need the same signed request from any signed-in person; a person whose role is not an
administrator's sees only the resources of their own account that IAM lets them list: for each resource, the
service's matching plural `List`, `Describe` or `Get` action for its type (for example `iam:ListRoles`).
Types without a matching listing action or resource ARN are hidden in enforce mode. Listing does not grant access to
resource details: in enforce mode, non-admin inventory omits `details`, including parameter values and IAM policy
documents. Use the authorized service APIs to read those details. Source IP, TLS, current time, region and principal/resource tag
conditions are evaluated for the inventory too. With `authorization: soft` or `off` they see their whole account. Only `health` (and `/_localstack/health`) stays open, so
container health checks keep working. The console itself shows only a TinyStack sign-in page until someone signs in, with
**Sign in with SSO** and a link to sign in with root access keys; after an SSO sign-in the person returns to the page
they first asked for.

Access-key sign-in is unchanged: root keys and root session credentials still connect with **Console session**, and
IAM users and ordinary assumed roles still cannot. Console actions that such credentials sign directly are evaluated by
IAM like any AWS API call.

## Command line sign-in

`tinystack login` gives the AWS CLI and SDKs the same per-person role credentials. It uses the OAuth device
authorization flow ([RFC 8628](https://datatracker.ietf.org/doc/html/rfc8628)) with the `console.auth.oidc` settings of
the configuration TinyStack runs with, then sends the resulting ID token to the running TinyStack, which verifies it,
maps the groups exactly as for the console, and returns the role's credentials as `credential_process` JSON:

```ini
# ~/.aws/config
[profile tinystack-dev]
credential_process = tinystack login --profile tinystack-dev --config /path/to/tinystack.yaml
region = us-east-1
endpoint_url = http://127.0.0.1:4566
```

The first AWS CLI call prints a URL on standard error; open it, sign in, and the call continues. The credentials are cached
under your user cache directory (`tinystack/sso/<profile>.json`, atomically replaced with mode 0600) until five minutes before they expire, so
later calls do not ask again. `--endpoint` overrides the console URL; the default is the origin of `redirect_url`. Remote endpoints require
HTTPS; HTTP is allowed only for localhost and loopback addresses. Discovery, key and token endpoints use the same
transport rule and do not follow HTTP redirects.

The provider's client must allow the device grant (the example client lists
`urn:ietf:params:oauth:grant-type:device_code`), and the machine running the command must read `client_secret_file`.
TinyStack refuses an ID token issued in the future or at least five minutes ago and redeems each ID token once, and the endpoint (`POST /_tinystack/auth/device`) rejects
requests carrying an `Origin` header, so browsers cannot use it. Note that a confidential client's secret is then
available to everyone who can run the command: use a public client (no `client_secret_file`) where the provider allows
it.

## Example with Authelia

`examples/sso` runs TinyStack with [Authelia](https://www.authelia.com/) 4.39 as the provider.

```sh
cd examples/sso
docker compose up --build
```

Everything in it is a **development-only** secret. Authelia has no plain-HTTP mode for its portal (its session cookie
domain must have a dot and not be `localhost`, and its `authelia_url` must be HTTPS), so the example puts Caddy with
its internal CA in front of it at `https://auth.tinystack.localhost:9443`. TinyStack reaches that name through a compose
network alias and trusts the CA through `SSL_CERT_FILE`. Your browser resolves `*.localhost` to your machine by
itself, and has to accept the certificate once: open `https://auth.tinystack.localhost:9443` and proceed past the
warning, or trust the CA after exporting it with
`docker run --rm -v tinystack-sso_ca:/ca alpine cat /ca/root.crt > caddy-root.crt`.

| Piece | File |
| --- | --- |
| Authelia: one client `tinystack-console`, `one_factor`, PKCE `S256` required, scopes `openid profile email groups`, a claims policy that puts `groups`, `email` and names in the ID token (Authelia 4.39 omits them otherwise), an RSA signing key that the `keygen` service generates on first start | `authelia/configuration.yml`, `compose.yml` |
| Users `admin` (`platform-admins`, password `admin-dev-password`) and `dev` (`team-b-dev`, password `dev-dev-password`) | `authelia/users.yml` |
| Client secret: hashed (PBKDF2) in Authelia's config, plain text for TinyStack's `client_secret_file` | `authelia/configuration.yml`, `secrets/oidc-client-secret` |
| TinyStack: `verify`, `enforce`, `claim: groups`, both mappings | `tinystack.yaml` |
| Roles `Admin` (everything, account `000000000000`) and `Developer` (S3 and SQS only, account `111111111111`), created by a ready hook | `init/ready.d/10-sso-roles.sh` |

The redirect URI is `http://127.0.0.1:4567/_tinystack/auth/callback` (Authelia accepts plain HTTP redirects to
loopback), so open the console at `http://127.0.0.1:4567`, not `localhost`. Try it:

1. Choose **Console session**, **Sign in with SSO**, and sign in as `dev`. Create an S3 bucket: allowed. Open IAM:
   denied with an explanation naming the missing permission. Sign out.
2. Sign in as `admin` and open **Request inspector**: it lists the developer's denied `iam:GetUser`. Signed requests
   with the root key `dev-root-a` / `dev-root-secret` work for the AWS CLI and for the administrative endpoints.

`docker compose down -v` removes everything.

## Other providers

Register a confidential client with the authorization code flow and PKCE, set its redirect URI to `redirect_url`, and
make the ID token carry a groups claim.

- **Zitadel**: enable "Assert Roles on Authentication" on the project and use the claim
  `urn:zitadel:iam:org:project:roles` (add the scope `urn:zitadel:iam:org:project:id:<project id>:aud` for the
  audience). It is an object keyed by role name, and an object's keys are read as the groups, so the role names are
  the keys of `roles`. Or add an Action that emits a `groups` array.
- **Keycloak**: add a *Group Membership* mapper to the client scope, claim name `groups`, with *Full group path* off
  and *Add to ID token* on. Or use realm roles with `claim: realm_access.roles`.
- **Authentik**: add a scope mapping that returns `{"groups": [g.name for g in request.user.ak_groups.all()]}` to the
  provider and request the `groups` scope.
- **Okta**: add a *Groups claim* named `groups` to the authorization server (or the app), filtered to the groups you
  map, included in the ID token.
- **Microsoft Entra ID**: the `groups` claim carries group object IDs (GUIDs), so the keys of `roles` are those GUIDs,
  unless the claim is configured to emit group names for cloud-only groups. Large memberships return an overage
  indicator instead of a list; filter the emitted groups.
- **Google Workspace** has no groups claim and is not supported.

## Security notes

- The authorization code flow uses PKCE `S256`, a random `state` and `nonce` held server side, a single-use login
  transaction bound to the browser by an HttpOnly cookie (Secure, `__Host-` prefixed and `Path=/` when
  `redirect_url` is HTTPS), and fixed redirect
  targets, so there is no open redirect.
- Tokens from the provider are used once and dropped; TinyStack stores no users or refresh tokens. The client secret is
  read from a file and is never logged.
- Credentials travel only in the bodies of same-origin `no-store` responses and stay in the tab's memory.
- The ID token is verified by the `go-oidc` library; TinyStack does not parse JWT signatures itself.
- Serve the console over HTTPS outside a loopback development setup; `issuer` and `redirect_url` refuse plain HTTP
  for other hosts.
- Mapped group names are matched exactly. Prefer groups that only administrators can edit at the provider.

- ID tokens require a nonempty subject, an issued-at time no later than TinyStack's clock, and an authorized party
  (`azp`) matching `client_id` when present; multiple audiences require `azp`. Keep provider and server clocks in sync.
- Sign-in transactions are capped at 32 pending transactions per socket source address, and 1024 globally.
  Signed-in sessions and five-minute device redemption records are also capped at 32 per verified subject and 1024
  globally. Repeating sign-in in one browser replaces its previous transaction. Pending-transaction exhaustion at
  `GET /_tinystack/auth/login` returns 429; session exhaustion at `GET /_tinystack/auth/callback` returns 503;
  redemption-record exhaustion at `POST /_tinystack/auth/device` returns 401 `InvalidToken`. The server does not
  trust `X-Forwarded-For`, so a reverse proxy or shared NAT shares that source limit.
- When terminating HTTPS at a reverse proxy, preserve the public `Host` header and set `redirect_url` to the public
  HTTPS origin. Cookie-bearing session/logout POSTs require that exact Origin or same-origin Fetch Metadata. Forwarded
  protocol headers never determine the trusted origin or IAM's `aws:SecureTransport` value. Origin checks compare
  schemes, case-insensitive DNS hosts and effective ports; explicit HTTPS `:443` and HTTP `:80` are equivalent
  to their omitted default ports. Other ports and schemes remain distinct.
- Browser console actions use the configured public console origin. Signed programmatic actions without Origin or
  Fetch Metadata can use either the console or AWS listener; signature, account/region scope and IAM checks still apply.
- Deleting a mapped role invalidates its federated credentials; recreating the same ARN does not revive old sessions.
- Console API invocation evaluates `execute-api:Invoke` against
  `arn:aws:execute-api:<region>:<account>:<api>/<stage>/<method>/<path>`, including route-specific explicit Deny.
- Sign-out immediately drops in-memory credentials and cancels pending collection. Changing the signed-in principal
  resets the console's inventory cache.

## STS web identity signatures

Independently of single sign-on, `AssumeRoleWithWebIdentity` in `identity.mode: verify` now checks the token's
signature against the keys its issuer publishes (OpenID discovery, then JWKS, with a five second timeout and a five
minute cache) before it evaluates the role's trust policy. Only issuers registered as IAM OIDC providers are contacted.
Unsigned tokens (`alg: none`), signatures by other keys and unknown key IDs are rejected with `InvalidIdentityToken`.
Tokens of Cognito identity pools, which TinyStack issues itself, verify against the pool's own key without any network
access. Parse mode keeps trusting the claims without checking signatures.
