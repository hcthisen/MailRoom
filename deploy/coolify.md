# Deploying Mailroom with Coolify

Use this instead of [README.md](README.md) (plain VPS) when the server already runs [Coolify](https://coolify.io).

| | Plain VPS ([README.md](README.md)) | Coolify (this file) |
|---|---|---|
| HTTPS certificate | Mailroom itself (Let's Encrypt, ports 80/443) | **Coolify's proxy**. Mailroom's own HTTPS is off (`HTTP_ONLY=1`) |
| Ports | 80 and 443 on the host | **None on the host**: the container listens on 8080 inside Docker, and Coolify routes your domain to it. Other services on the server are not affected |
| Settings | `/opt/mailroom/.env` | Coolify's environment variables |
| Data | `/opt/mailroom/data` | The Docker volume `mailroom-data` |
| CLI | `docker compose run/exec` over ssh | Not needed: the admin and the API token come from environment variables at first start, and the SES/DNS commands run on your own machine |

The repository's [docker-compose.yaml](../docker-compose.yaml) is made for this. It builds `app/Dockerfile`, publishes no ports, and sets `HTTP_ONLY=1`, `HTTP_ADDR=:8080` and `TRUSTED_PROXIES` for you.

---

## 1. Before you start

- A Coolify server (v4) whose proxy (Traefik or Caddy) is running, and its public IPv4 (and IPv6, if any).
- Coolify can read this repository. For a private GitHub repository: in Coolify, **Sources → + Add → GitHub App** and give the app access to the repository (or add a deploy key under **Keys & Tokens**).
- The SES and DNS commands below run on your own machine with Go 1.26+ (`go version`). They are the same commands as on a plain VPS, run with `go run`.

Run every command below from the repository root, with a shell where the values from `.env` are loaded (e.g. `set -a; . ./.env; set +a`).

## 2. Choose the address and the bootstrap values

1. The domain Mailroom runs on: `https://APP_SUBDOMAIN.DOMAIN`, e.g. `https://newsletter.example.com`. That is `BASE_URL`.
2. Generate an admin password and an API token, and keep both in `secrets/` (never in git):

   ```bash
   mkdir -p secrets
   printf 'mr_%s\n' "$(head -c 32 /dev/urandom | base32 | tr -d '=\n' | tr 'A-Z' 'a-z')" > secrets/api-token.txt
   head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > secrets/admin-password.txt
   ```

   The token must be `mr_` followed by 52 characters `a-z`/`2-7`; the command above makes exactly that.

## 3. Provision SES (on your machine)

`provision ses` uses the **setup** keys (the `setup-agent` user) once, creates everything in AWS, and writes the **sending** user's values to a local file instead of the server:

```bash
touch secrets/mailroom-server.env && chmod 600 secrets/mailroom-server.env
BASE_URL=https://newsletter.example.com \
  go run ./app/cmd/mailroom provision ses --env-file secrets/mailroom-server.env --json > secrets/provision-ses.json
```

(`DOMAIN`, `APP_SUBDOMAIN`, `MAIL_FROM_SUBDOMAIN`, `OWNER_EMAIL`, `AWS_REGION` and the setup keys come from the loaded `.env`.) `secrets/mailroom-server.env` now holds `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `SNS_TOPIC_ARN`, `SES_CONFIGURATION_SET` and `BACKUP_BUCKET` of the sending user. These go into Coolify in step 4. The production-access request (`--request-production --use-case FILE`) also runs here, the same way.

## 4. Create the application in Coolify

### By hand

1. **Projects → (your project) → + New → Private Repository (with GitHub App)**, or **Public Repository** if the repo is public. Select this repository and branch `main`.
2. **Build Pack: Docker Compose**, Base Directory `/`, Docker Compose Location `/docker-compose.yaml`. Save.
3. **Domains for mailroom:** `https://newsletter.example.com:8080`. The `:8080` is the **container** port, not a public one: Coolify serves the domain on 443 with its own certificate and forwards to port 8080 inside Docker.
4. **Environment Variables:** set
   - the sender values from `.env`: `DOMAIN`, `FROM_NAME`, `FROM_EMAIL`, `REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`, `TIMEZONE`, `APP_SUBDOMAIN`, `MAIL_FROM_SUBDOMAIN`,
   - `BASE_URL=https://newsletter.example.com`,
   - every line of `secrets/mailroom-server.env`,
   - `MAILROOM_ADMIN_EMAIL` (= `OWNER_EMAIL`), `MAILROOM_ADMIN_PASSWORD` (from `secrets/admin-password.txt`) and `MAILROOM_BOOTSTRAP_TOKEN` (from `secrets/api-token.txt`).

   Never put the **setup** keys or the Cloudflare/Hetzner tokens here.
5. Don't deploy yet: do the DNS first (step 5), so the certificate can be issued.

### Through Coolify's API (for an agent)

With `COOLIFY_URL` and an API token (`COOLIFY_API_TOKEN`, created under **Keys & Tokens → API tokens** with write access), the same steps are API calls on `$COOLIFY_URL/api/v1` with `Authorization: Bearer $COOLIFY_API_TOKEN`. Field names change between Coolify versions, so check them against the API reference of the installed version (`$COOLIFY_URL/docs/api` or coolify.io/docs/api-reference) before each call:

1. `GET /servers`, `GET /projects` (or `POST /projects`) to find `server_uuid`, `project_uuid` and the environment name.
2. Create the application: `POST /applications/private-github-app` (with `github_app_uuid`), `/applications/private-deploy-key` or `/applications/public`, with `git_repository`, `git_branch: main`, `build_pack: dockercompose`, `docker_compose_location: /docker-compose.yaml` and `instant_deploy: false`.
3. Set the domain: `PATCH /applications/{uuid}` with `docker_compose_domains: [{"name": "mailroom", "domain": "https://newsletter.example.com:8080"}]`.
4. Set the variables of step 4.4: `PATCH /applications/{uuid}/envs/bulk` (or one `POST /applications/{uuid}/envs` per variable). Mark the secrets as such if the version supports it.
5. Deploy (after DNS): `GET /deploy?uuid={uuid}`, then follow `GET /deployments/{deployment_uuid}` until it is finished, and `GET /applications/{uuid}/logs` on failure.

## 5. DNS

The records are the same as on a plain VPS, with the **Coolify server's** IP for the A/AAAA record:

```bash
# reads the DKIM tokens with the sending keys
set -a; . secrets/mailroom-server.env; set +a
go run ./app/cmd/mailroom dns records --ipv4 COOLIFY_IPV4 --ipv6 COOLIFY_IPV6
# or with Cloudflare (prints the plan; add --yes after the customer's yes)
go run ./app/cmd/mailroom provision dns --cloudflare --ipv4 COOLIFY_IPV4 --ipv6 COOLIFY_IPV6
```

If the Coolify server already has a wildcard record for `*.DOMAIN`, the A/AAAA record is already there; add the DKIM and MAIL FROM records only.

## 6. Deploy and check

1. Deploy in Coolify (**Deploy**, or the API call above). The first build takes a few minutes (Go compile).
2. Coolify shows the container as healthy when Mailroom's health check passes. Open `https://newsletter.example.com/admin/` and sign in with `OWNER_EMAIL` and the admin password.
3. With the bootstrap token: `curl -H "Authorization: Bearer $(cat secrets/api-token.txt)" https://newsletter.example.com/api/v1/me`, then `GET /api/v1/system/doctor`. The `https_cert` check is `skipped` (Coolify handles HTTPS); the SNS subscription is `ok` once Mailroom has subscribed `/hooks/ses` (a minute after start).
4. **Remove** `MAILROOM_ADMIN_PASSWORD` from Coolify's variables (the admin already exists). Keep `MAILROOM_BOOTSTRAP_TOKEN` only while an agent needs the API; to retire it, revoke the token named `bootstrap` (`DELETE /api/v1/tokens/{id}` or the UI) and delete the variable.

## Operations

| Task | How |
|---|---|
| Update Mailroom | Push to `main` and redeploy in Coolify. Migrations run on start, after a backup. |
| Change a setting | Business settings (sender, footer, tracking…) through the UI or `PATCH /api/v1/settings`. Infrastructure values (AWS keys, BASE_URL) in Coolify's variables, then redeploy. |
| Logs | Coolify → the application → Logs. |
| Backups | Nightly into the volume (`/data/backups`) and to `BACKUP_BUCKET` in S3 if set. Coolify's own volume backups can be added on top. |
| Restore | Stop the application in Coolify. On the Coolify server, find the volume and image (`docker volume ls`, `docker image ls`, both named after the application) and run `docker run --rm -e DOMAIN=example.com -v <volume>:/data <image> restore /data/backups/<file>`. Start the application again; it migrates if needed. |
| Rotate the sending key | `provision ses --rotate-keys` on your machine as in step 3, put the new values in Coolify, redeploy. |
