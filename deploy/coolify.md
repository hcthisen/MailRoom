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

Use normal Coolify Compose deployment so its proxy and resource-specific
network are generated. The reusable file does not join a shared network or
declare a cross-resource alias. If an existing installation relies on an
internal route from another app, review that dependency before adopting this
file and maintain an installation-specific override with a unique alias.
Enable a predefined shared network only for that intentional connection;
verify the installed version's generated routing. See [Coolify networking](https://coolify.io/docs/applications/builds/docker-compose#compose-networking).

Steps 1–6 below describe a **new installation**. For an existing Mailroom,
use [the migration procedure](#migrate-an-existing-vps-installation) instead.
Read [agent deployment notes](agent-notes.md) before choosing AWS resources.

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

(`DOMAIN`, `APP_SUBDOMAIN`, `MAIL_FROM_SUBDOMAIN`, `OWNER_EMAIL`, `AWS_REGION` and the setup keys come from the loaded `.env`.) `secrets/mailroom-server.env` now holds `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `SNS_TOPIC_ARN`, `SES_CONFIGURATION_SET`, `BACKUP_BUCKET`, `INBOUND_BUCKET` and `INBOUND_DOMAIN` of the sending user.

Receiving email is set up by default (Mailroom's Inbox gets all mail sent to addresses on `DOMAIN`). It is left off, with a warning in the JSON's `warnings`, when `DOMAIN` already has MX records pointing elsewhere (an existing mailbox, which is never replaced), another SES receipt rule set is active, or the region has no SES receiving; `--no-receive` turns it off. When it is off, `INBOUND_BUCKET` and `INBOUND_DOMAIN` are empty: leave them out of Coolify. These go into Coolify in step 4. The production-access request (`--request-production --use-case FILE`) also runs here, the same way.

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

If the owner already saved that token as `COOLIFY_API_KEY`, accept it as an
input alias and normalize it privately to `COOLIFY_API_TOKEN`; do not ask for
another credential. If the public API is denied by a WAF/security policy,
respect the denial and use the owner's authorized browser interface for
supported operations. Do not spoof a banned client, bypass a challenge or
weaken the policy. An existing browser server terminal can perform the
authorized file/image migration without introducing new SSH credentials.

1. `GET /servers`, `GET /projects` (or `POST /projects`) to find `server_uuid`, `project_uuid` and the environment name.
2. Create the application: `POST /applications/private-github-app` (with `github_app_uuid`), `/applications/private-deploy-key` or `/applications/public`, with `git_repository`, `git_branch: main`, `build_pack: dockercompose`, `docker_compose_location: /docker-compose.yaml` and `instant_deploy: false`.
3. Set the domain: `PATCH /applications/{uuid}` with `docker_compose_domains: [{"name": "mailroom", "domain": "https://newsletter.example.com:8080"}]`.
4. Set the variables of step 4.4: `PATCH /applications/{uuid}/envs/bulk` (or one `POST /applications/{uuid}/envs` per variable). Mark the secrets as such if the version supports it.
5. Deploy (after DNS) using the installed version's documented method; for versions exposing `POST /deploy?uuid={uuid}`, send an empty JSON body. Follow its deployment status endpoint until finished and inspect application logs on failure. Verify methods and request schemas against the installed API before acting; these examples are not a version guarantee.

## 5. DNS

The records are the same as on a plain VPS, with the **Coolify server's** IP for the A/AAAA record:

```bash
# reads the DKIM tokens with the sending keys
set -a; . secrets/mailroom-server.env; set +a
go run ./app/cmd/mailroom dns records --ipv4 COOLIFY_IPV4 --ipv6 COOLIFY_IPV6
# or with Cloudflare (prints the plan; add --yes after the customer's yes)
go run ./app/cmd/mailroom provision dns --cloudflare --ipv4 COOLIFY_IPV4 --ipv6 COOLIFY_IPV6
```

If the Coolify server already has a wildcard record for `*.DOMAIN`, the A/AAAA record is already there; add the DKIM and MAIL FROM records only. With receiving on, the list also has `MX DOMAIN 10 inbound-smtp.REGION.amazonaws.com`; once it exists, send a test email to any address on the domain and check that it shows up under **Inbox** in the admin within a minute.

## 6. Deploy and check

1. Deploy in Coolify (**Deploy**, or the API call above). The first build takes a few minutes (Go compile).
2. Coolify shows the container as healthy when Mailroom's health check passes. Open `https://newsletter.example.com/admin/` and sign in with `OWNER_EMAIL` and the admin password.
3. With the bootstrap token: `curl -H "Authorization: Bearer $(cat secrets/api-token.txt)" https://newsletter.example.com/api/v1/me`, then `GET /api/v1/system/doctor`. The `https_cert` check is `skipped` (Coolify handles HTTPS); the SNS subscription is `ok` once Mailroom has subscribed `/hooks/ses` (a minute after start).
4. **Remove** `MAILROOM_ADMIN_PASSWORD` from Coolify's variables (the admin already exists). If retiring the newly created bootstrap token is part of the agreed handover, identify that exact token and revoke it (`DELETE /api/v1/tokens/{id}` or the UI), then delete its variable. Removing a variable alone does not revoke a token. Preserve any token the owner chose to retain for future assistance. A migrated database needs no bootstrap variables; do not revoke its existing `setup-agent` or other tokens as migration cleanup.

## Migrate an existing VPS installation

This moves the existing installation; it is not a new setup. Do not run
`provision ses`, `provision dns`, key rotation, contact import or admin/token
bootstrap. Preserve the current AWS region, sender keys, identity,
configuration set, SNS topic/subscription, backup/inbound buckets and receipt
rules, including any installation-specific isolation. Keep the same `BASE_URL`
so existing form, unsubscribe, tracking, confirmation and webhook URLs work.
Changing hosting does not require changing the sending domain or incoming MX.

Proceed only within explicit migration authorization, including the brief
write freeze, app-host DNS replacement and rollback plan. Respect any
installation-specific checkpoints. Record progress and non-secret resource
identifiers in its operations notes; never put its credentials in this guide.

### Prepare without starting a second production instance

1. Read the source installation's runtime configuration and operations notes
   privately. Record deployed source/image version, database schema, totals
   by contact status, forms, Inbox records, active/scheduled broadcasts,
   enrolments, pending jobs, queued/in-flight messages and sending state.
2. Create the Coolify resource with instant/automatic deployment disabled.
   Verify the installed Coolify API schema rather than assuming the fresh
   setup examples match it. Build the **same deployed source version** or
   reuse its verified image; combine an upgrade with migration only under a
   separate reviewed plan. If transferring with `docker save`/`load`, first
   tag and export the source image under an installation-specific name, such
   as `mailroom-example:migration`, to preserve an unambiguous target tag.
   Different Docker image stores can report an OCI index ID on the source
   and a classic image configuration ID after loading. Do not reject the
   transfer based only on unequal raw `.Id` values: verify archive integrity,
   ordered `RootFS.Layers` diff IDs, OS/architecture and image configuration
   (`Entrypoint`, `Cmd`, `User`, `Env`, `WorkingDir`) on both sides. Compare
   configuration privately without printing environment values; investigate
   any actual layer/platform/configuration mismatch before proceeding.
3. Inspect rendered Compose and routing. Use a unique network alias and
   inspect the actual persistent volume name/mount; Coolify may prefix names.
   Do not reuse another application's alias or volume. Keep one replica,
   internal port 8080, no host ports and appropriate trusted proxy ranges.
   The generated Compose may declare its new stack network `external` even
   before first deployment. A pre-deployment one-shot restore then fails if
   that network does not exist. Inspect the generated definition and existing
   networks; create only the exact missing network owned by this new resource,
   with the reviewed settings. Do not recreate or alter shared proxy networks
   or another application's network to make the restore run.
4. Prepare only the existing runtime fields in Coolify's private environment:
   same `DOMAIN`, `BASE_URL`, region, sending credentials and resource names;
   `HTTP_ONLY=1`, `HTTP_ADDR=:8080`, `MAILROOM_DATA_DIR=/data`. Do not include
   setup keys or `MAILROOM_ADMIN_*`/`MAILROOM_BOOTSTRAP_TOKEN`. If the original
   uses an explicit `MAILROOM_SECRET`, preserve and explicitly map it in
   Compose; otherwise preserve its generated `data/secret.key`.
5. Build only; **do not start a production-data clone while the VPS runs**.
   Every `serve` starts background jobs, schedulers and subscription workers.
   Omitting sender keys is not a read-only staging mode, and a sending pause
   does not stop other database writes. If the platform cannot build without
   starting, use a separate controlled build path. Check proxy/certificate
   readiness without exposing a second authoritative Mailroom database.

### Freeze, snapshot and populate the stopped target

1. Preserve the original sending state. Use the API to apply a manual global
   sending pause for migration; confirm in-flight sends have completed.
   Gracefully stop the old container and verify it remains stopped. This
   freezes public forms, webhook writes, owner edits and all workers too;
   a sending pause alone does not provide a complete snapshot boundary.
2. With the server stopped, run the existing image's one-shot
   `mailroom backup --json` command. It creates a consistent `VACUUM INTO`
   snapshot and can upload it to the existing backup bucket. Verify completion
   and `s3_key`/`secret_key_s3_key` when applicable. Do not copy a live
   `mailroom.db` alone: committed changes may still be in its WAL.
3. Transfer the final snapshot, original signing key, media and any required
   import files into private target storage; retain a complete source recovery
   copy and its runtime `.env`. Copy the original key **before any target
   CLI, doctor or first start**, since config loading creates a new key when
   absent. Verify checksums without displaying contents. Set data ownership
   to uid/gid 65532, directories 700 and secret/database files 600.
4. While target service is stopped, use its same-version image and exact
   volume to `restore` the final snapshot. Preserve environment-file parsing:
   Compose `--env-file` handles quoted dotenv values, while `docker run
   --env-file` can retain those quotes literally. Passing a quoted runtime
   file directly to `docker run` can fail configuration validation before
   database restore. Prefer the generated Compose's one-shot `run --rm -T
   --no-deps` with its correctly parsed environment and intended volume. If
   invoking Docker directly, parse the known dotenv format into a subprocess
   environment with a compatible parser, reject unsupported syntax, and pass
   only environment names (`-e NAME`), never secret values in argv. Do not
   print resolved Compose configuration or source an untrusted file as shell
   code. See [Compose environment-file syntax](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/)
   and [Docker run environment options](https://docs.docker.com/reference/cli/docker/container/run/#env).
   Check the command's result rather than assuming a started one-shot restored
   anything. Restore validates SQLite/schema,
   removes stale WAL/SHM files and leaves all sending paused. Verify success
   before starting. Existing admins/API tokens remain; browser sessions end
   during restore, so the owner signs in again. An empty replacement has no
   newer database from which to recover opt-outs: the final frozen snapshot
   must contain the latest authoritative data.

### Validate, cut over and retain a usable rollback

1. Start only the target, with the source still stopped. Test the intended
   production hostname against the new proxy using Host/SNI and normal TLS
   verification, such as `curl --resolve HOST:443:NEW_IP https://HOST/healthz`.
   Arrange a supported certificate issuance path or an agreed brief cutover
   window; do not disable TLS verification or change `BASE_URL` to a temporary
   host. An HTTP-only doctor's skipped certificate check does not verify
   the public proxy's certificate.
2. Compare baseline data, `/api/v1/me`, doctor, form pages, media, Inbox and
   the preserved signing-key checksum. Confirm no unexpected pending sends
   or jobs. The existing SNS endpoint URL remains the same. Keep sending
   paused throughout validation; do not send to contacts for a test.
3. After the approved checks, replace only the actual app host's A/AAAA
   records. Update or remove a stale AAAA as reviewed; it must not route some
   clients to the old database. Leave SES/MX/DKIM/MAIL FROM records unchanged.
   Revalidate current DNS values immediately before replacement.
4. Handle cached old-IP traffic explicitly: either use a tested temporary
   forwarding path from the old address to the new proxy, or disclose the
   temporary interruption. Never restart the stale Mailroom to accept forms
   or callbacks. Confirm SNS retry/delivery and external inbound behavior,
   public DNS/TLS and owner sign-in on the final URL.
5. Take a fresh target backup and verify off-server storage plus private
   runtime/key/media recovery copies. Review the queued-recipient count
   before lifting the migration pause under the applicable sending approval;
   preserve a pre-existing manual pause. Keep the source stopped and retain
   its recovery files until handover/retirement is agreed.

Before the target accepts writes, rollback can return to the frozen source.
After **any** target writes, its database is authoritative: freeze/stop target,
take its latest consistent snapshot and transfer/reconcile it back before
switching routing. Simply reverting DNS to the old snapshot loses opt-outs,
inbound messages and form submissions and can repeat sends. At every point,
run only one authoritative set of Mailroom workers.

## Operations

| Task | How |
|---|---|
| Update Mailroom | Push to `main` and redeploy in Coolify. Migrations run on start, after a backup. |
| Change a setting | Business settings (sender, footer, tracking…) through the UI or `PATCH /api/v1/settings`. Infrastructure values (AWS keys, BASE_URL) in Coolify's variables, then redeploy. |
| Logs | Coolify → the application → Logs. |
| Backups | Nightly into the volume (`/data/backups`) and to `BACKUP_BUCKET` in S3 if set. Coolify's own volume backups can be added on top. |
| Restore | Stop the application in Coolify. On the Coolify server, find the volume and image (`docker volume ls`, `docker image ls`, both named after the application) and run `docker run --rm -e DOMAIN=example.com -v <volume>:/data <image> restore /data/backups/<file>`. Start the application again; it migrates if needed. |
| Rotate the sending key | Only for an installation that owns the default AWS resources: `provision ses --rotate-keys` as in step 3, update Coolify, redeploy. For shared-account isolated resources, follow that installation's recorded key plan instead. Migration itself needs no rotation. |
