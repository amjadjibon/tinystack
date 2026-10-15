# Console single sign-on with Authelia

A development stack: TinyStack signing people in through [Authelia](https://www.authelia.com/) (OpenID Connect), with
Caddy providing the TLS that Authelia requires. **Every secret in this directory is public and for development only.**

```sh
docker compose up --build
```

Open <http://127.0.0.1:4567> (not `localhost`) and choose **Sign in with SSO**. Users:
`dev` / `dev` (S3 and SQS only) and `admin` / `admin` (administrator). The full walkthrough,
the TLS notes and other providers are in [the guide](../../docs/site/guides/sso.md).

The `admin` demo user belongs to both `platform-admins` and `team-b-dev`. After sign-in, choose Admin
in account `000000000000` or Developer in account `111111111111`; use the console Account menu to switch.
For `tinystack login`, select one with `--role arn:aws:iam::000000000000:role/Admin` (or the Developer ARN).
