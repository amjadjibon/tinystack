# Console single sign-on with Authelia

A development stack: TinyStack signing people in through [Authelia](https://www.authelia.com/) (OpenID Connect), with
Caddy providing the TLS that Authelia requires. **Every secret in this directory is public and for development only.**

```sh
docker compose up --build
```

Open <http://127.0.0.1:4567> (not `localhost`), choose **Console session** and **Sign in with SSO**. Users:
`dev` / `dev-dev-password` (S3 and SQS only) and `admin` / `admin-dev-password` (administrator). The full walkthrough,
the TLS notes and other providers are in [the guide](../../docs/site/guides/sso.md).
