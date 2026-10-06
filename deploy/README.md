# Deploying Mailroom

> On a server that runs **Coolify**, don't use this file: deploy the repository's root `docker-compose.yaml` as described in [coolify.md](coolify.md). Coolify then handles HTTPS and no host ports are used.

Mailroom runs as one container, built on the server from the source in the
kit's `app/` directory. The compose file in this directory,
[`docker-compose.yml`](docker-compose.yml), starts it. The runbook in
[`../AGENTS.md`](../AGENTS.md) (phases 3, 4 and 5) uses the commands below.

## Layout on the server

Everything lives in `/opt/mailroom/`:

```text
/opt/mailroom/
  docker-compose.yml   copied from the kit's deploy/ directory
  README.md            this file (copied along; optional)
  app/                 the kit's app/ directory, with vendor/
  .env                 Mailroom's settings and the sending keys (mode 600)
  data/                database, media, backups, secret key, certificates
```

The compose file sits **next to** `app/`, not in a `deploy/` subdirectory.
So when AGENTS.md says "copy `app/` and `deploy/` to `/opt/mailroom/`",
copy `app/` as a directory and the **contents** of `deploy/`. That way
every `docker compose …` command in the runbook works as written, run in
`/opt/mailroom/`.

The container runs as the image's non-root user, uid/gid **65532**. That
user must own `data/` and `.env`: Mailroom writes its database to `data/`,
and `mailroom provision ses` writes the sending keys to `.env`. Because
`.env` is then readable only by uid 65532, run the `docker compose` commands
with `sudo`. To run the container as your own user instead, see
[Running without sudo](#running-without-sudo).

## 1. Copy the kit to the server

From the kit's directory on your machine (`SERVER` is the SSH login, e.g.
`admin@203.0.113.10`):

```bash
ssh SERVER 'sudo install -d -m 0755 -o "$(id -u)" -g "$(id -g)" /opt/mailroom'
rsync -a --delete app/ SERVER:/opt/mailroom/app/
rsync -a deploy/ SERVER:/opt/mailroom/
```

The trailing slash in `deploy/` copies the directory's contents, so
`docker-compose.yml` ends up in `/opt/mailroom/`. That second copy has no
`--delete`, so it never touches `.env` or `data/`. Don't leave `vendor/`
out: the image builds from the vendored Go modules and doesn't download
any.

Without rsync (e.g. on Windows, which has `tar` and `ssh` built in), copy
with tar instead. For an upgrade, first remove the old copy with
`ssh SERVER 'rm -rf /opt/mailroom/app'`, so deleted files don't linger:

```bash
tar -czf - app | ssh SERVER 'tar -xzf - -C /opt/mailroom'
tar -czf - -C deploy docker-compose.yml README.md | ssh SERVER 'tar -xzf - -C /opt/mailroom'
```

## 2. Create `.env` and `data/`

Write the file without an editor, so it also works over a non-interactive
`ssh` (an agent can't drive `sudoedit`). From your machine, send the lines
through ssh's standard input:

```bash
ssh SERVER 'cd /opt/mailroom && sudo install -m 600 /dev/null .env && sudo tee .env >/dev/null' <<'EOF'
DOMAIN=example.com
FROM_NAME="Example Ltd"
FROM_EMAIL=news@example.com
REPLY_TO_EMAIL=hello@example.com
COMPANY_FOOTER="Example Ltd · 1 Street, 1234 City · Reg. no. 12345678"
OWNER_EMAIL=owner@example.com
TIMEZONE=Europe/Copenhagen
APP_SUBDOMAIN=newsletter
MAIL_FROM_SUBDOMAIN=bounce
AWS_REGION=eu-central-1
EOF
```

Put in the customer's sender settings: `DOMAIN`, `FROM_NAME`, `FROM_EMAIL`,
`REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`, `TIMEZONE`,
`APP_SUBDOMAIN`, `MAIL_FROM_SUBDOMAIN` and `AWS_REGION`, one `NAME=value`
per line without comments. (Logged in on the server yourself, `sudoedit .env`
works too.) Setup keys (the AWS `setup-agent` user, Cloudflare, Hetzner)
never go into this file. The sender
settings (`FROM_*`, `REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`,
`TIMEZONE`) are copied into Mailroom's settings only once, by the first
`doctor` run or the first start; change them later with
`PATCH /api/v1/settings` or on the Settings page, not here. Then hand the
file and the data directory to the container user:

```bash
sudo chown 65532:65532 .env
sudo chmod 600 .env
sudo install -d -m 0700 -o 65532 -g 65532 data
```

Create `.env` **before** the first `docker compose` command. If it's
missing, Docker creates a directory named `.env` for the bind mount, and
nothing works until you remove that directory.

## 3. Build the image

```bash
cd /opt/mailroom
sudo docker compose build
```

This pulls `golang:1.26-alpine` (the build stage) and
`gcr.io/distroless/static-debian12:nonroot` (the runtime image, which has
CA certificates and time zone data but no shell), then compiles Mailroom
from `app/` with the vendored modules. To stamp a version into the binary
(`mailroom version`), set `MAILROOM_VERSION=1.0.0` in `.env` or in the
shell.

## 4. Provision SES

Pass the setup user's keys for this one run only, and **never on a command
line**: `sudo` writes every command it runs, with all its arguments, to the
system log (`/var/log/auth.log` and the journal), so
`sudo docker compose run -e AWS_SECRET_ACCESS_KEY=…` (or `sudo env NAME=…`)
would leave the setup key, which can grant any permission in the AWS
account, on the server's disk and in its disk backups. Pass the values
through the environment and give `docker compose` only the names. From your
machine, with the values in the shell variables `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` (read from your local `.env`), send them through
ssh's standard input:

```bash
printf '%s\n%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" | ssh SERVER '
  read -r AWS_ACCESS_KEY_ID; read -r AWS_SECRET_ACCESS_KEY
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
  cd /opt/mailroom && sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY \
    docker compose run --rm -T -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY mailroom provision ses --json'
```

`printf` is a shell builtin, so the values appear in no process list, and
the logged command holds only the names. Logged in on the server yourself,
type them in instead:

```bash
cd /opt/mailroom
read -r AWS_ACCESS_KEY_ID; read -rs AWS_SECRET_ACCESS_KEY; export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY \
  docker compose run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY mailroom provision ses --json
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
```

The same pattern passes `CLOUDFLARE_API_TOKEN` in step 5. The other setup
commands below show only the `sudo … docker compose` part; the values get
into the environment the same way.

The command writes the sending user's keys (`AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`), `AWS_REGION`, `SNS_TOPIC_ARN`,
`SES_CONFIGURATION_SET` and `BACKUP_BUCKET` (empty with
`--no-backup-bucket`) into `/opt/mailroom/.env`, which is bind-mounted into
the container at that same path, and prints the DNS records. `run` doesn't
publish any ports, so it also works while the server is running. A running
server reads `.env` only when its container is created, so afterwards run
`sudo docker compose up -d` (it recreates the container with the new
values). `sudo docker compose restart` is not enough: it keeps the old
environment.

Before it changes anything in AWS, the command checks that it can write
`.env`. If `.env` doesn't belong to the container user (see step 2), it
stops with a `chown` hint and nothing is created.

**Production access** (after CHECKPOINT 2): the use-case text (200 to
5,000 characters) must be readable inside the container, so copy it to the
server and mount it:

```bash
# on your machine
scp secrets/ses-use-case.txt SERVER:/opt/mailroom/ses-use-case.txt
# on the server
chmod 644 /opt/mailroom/ses-use-case.txt
# the keys in the environment, passed as above
sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY docker compose run --rm -T \
  -v /opt/mailroom/ses-use-case.txt:/tmp/ses-use-case.txt:ro \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  mailroom provision ses --request-production --use-case /tmp/ses-use-case.txt --json
```

The file needs mode 644. It isn't secret, but delete it afterwards anyway.

## 5. DNS

```bash
# Manual DNS: print the records as a table (add --dmarc only as described below)
sudo docker compose run --rm mailroom dns records --ipv4 SERVER_IPV4 --ipv6 SERVER_IPV6

# Cloudflare: show the diff first, then apply it after the customer's yes.
# CLOUDFLARE_API_TOKEN is in the environment, passed as in step 4.
sudo --preserve-env=CLOUDFLARE_API_TOKEN docker compose run --rm -T -e CLOUDFLARE_API_TOKEN mailroom provision dns --cloudflare --ipv4 SERVER_IPV4 --ipv6 SERVER_IPV6
sudo --preserve-env=CLOUDFLARE_API_TOKEN docker compose run --rm -T -e CLOUDFLARE_API_TOKEN mailroom provision dns --cloudflare --ipv4 SERVER_IPV4 --ipv6 SERVER_IPV6 --yes
```

Run these after `provision ses`: both read the DKIM tokens from the SES
identity with the sending keys it wrote to `.env`, so they need no setup
keys (Cloudflare needs its token for that run only). Without `--yes`,
`provision dns` changes nothing; it exits with code 2 when an existing
record conflicts (it never changes one).

`--ipv4` (and `--ipv6`) add the A (and AAAA) record for `APP_SUBDOMAIN`;
without them that record is left out, and the HTTPS certificate and checks
fail. Leave out `--ipv6` if the server has no IPv6 address. Only pass
`--dmarc` if `_dmarc.DOMAIN` doesn't exist yet and the customer agreed:
`dns records` doesn't look up DNS, so it adds the DMARC row whenever the
flag is given, and a second DMARC record makes DMARC invalid for the whole
domain (`provision dns --cloudflare` skips it when one exists).

IPv6 visitors keep their own address inside the container because the
compose file turns on IPv6 for its network (Docker Engine 27 or newer, as
installed from Docker's repository). On an older Docker, or if
`sudo docker compose up -d` reports an IPv6 error, leave out the AAAA
record (`--ipv6`) rather than the IPv6 network: otherwise every IPv6
visitor arrives from the same internal address and shares its rate limits.

Check the result with the doctor. It exits with code 2 while a check fails:

```bash
sudo docker compose run --rm mailroom doctor           # before the server runs
sudo docker compose exec mailroom mailroom doctor      # once it runs
```

`doctor`, `admin create` and `token create` create the database on a new
install, but never upgrade an existing one: after building a new version
they refuse (exit 1) until `sudo docker compose up -d` has started it, which
migrates after a backup.

## 6. Start and bootstrap

```bash
sudo docker compose up -d
sudo docker compose ps               # STATUS shows "healthy" after up to a minute
sudo docker compose logs -f mailroom # JSON logs
```

On start, Mailroom applies database migrations (after backing up the
database), gets its Let's Encrypt certificate for `APP_SUBDOMAIN.DOMAIN` on
ports 80/443, and subscribes `/hooks/ses` to the SNS topic.

```bash
sudo docker compose exec mailroom mailroom admin create --email OWNER_EMAIL
sudo docker compose exec mailroom mailroom token create --name setup-agent --scope admin
```

Each command shows the generated password or token once. Store them in
`secrets/`. To write the output straight to a file without showing it, add
`-T` (`sudo docker compose exec -T mailroom mailroom token create … > FILE`;
`token create` prints only the token on standard output). From here on,
use the API.

The health check runs `mailroom healthcheck` in the container every 30 s.
The command GETs `/healthz` on port 80, or on `HTTP_ADDR` with `HTTP_ONLY=1`,
because the image has no curl. To run it by hand, use
`sudo docker compose exec mailroom mailroom healthcheck`.

## Operations

| Task | Command (in `/opt/mailroom`) |
|---|---|
| Logs | `sudo docker compose logs --since 1h mailroom` |
| Restart (same settings) | `sudo docker compose restart mailroom` (keeps the environment the container was created with) |
| Apply a changed `.env` (settings, new keys) | `sudo docker compose up -d` (recreates the container) |
| Backup now | `sudo docker compose exec mailroom mailroom backup --json` |
| List backups | `sudo ls -l data/backups/` (nightly at 02:30, kept 14 days; also `GET /api/v1/system/backups`) |
| Restore | `sudo docker compose stop mailroom`<br>`sudo docker compose run --rm mailroom restore /data/backups/FILE.db`<br>`sudo docker compose up -d`, then check and resume sending (see [What a restore does](#what-a-restore-does)) |
| Change a setting in `.env` | `sudoedit .env` (or, without a terminal, `sudo sed -i 's/^NAME=.*/NAME=value/' .env`), then `sudo docker compose up -d` (it recreates the container with the new environment) |
| New sending key | with the setup keys in the environment (step 4): `sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY docker compose run --rm -T -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY mailroom provision ses --rotate-keys`, then at once `sudo docker compose up -d` (see below) |
| Version | `sudo docker compose exec mailroom mailroom version` |

**Rotating the sending key:** `--rotate-keys` creates a new access key for
`mailroom-sender`, writes it to `.env` and deletes the old one. The running
container still has the old key until it is recreated, so run
`sudo docker compose up -d` right after. If SES rejects the old key in
between, Mailroom pauses all sending (messages stay queued, nothing fails),
alerts the owner, and resumes by itself once the recreated container's
key works.

**If the sending key leaked** (e.g. a copy of `.env` got out), rotate it as
above, then check the doctor's `sns_subscription` check. The sending user
may subscribe only Mailroom's own `/hooks/ses` to the event topic, but a
subscription made before that rule (or by someone with other AWS access)
keeps receiving every recipient address after the rotation; the check lists
any other subscription, and you delete it in the SNS console. The backup
bucket is versioned, and the sending user can't delete versions, so a
backup or `keys/secret.key` it overwrote can be recovered from the
object's previous version in the S3 console.

**Upgrade or rebuild:** copy the new `app/` (step 1), then run:

```bash
sudo docker compose build && sudo docker compose up -d
```

Migrations run automatically at start, and the database is backed up first.
If you change `app/go.mod`, run `go mod vendor` in `app/` before copying, so
that `vendor/` matches.

## Backups and disaster recovery

The nightly backup (02:30, kept 14 days in `data/backups/`) is a copy of the
database. With `BACKUP_BUCKET` set, each backup is also uploaded to
`s3://BACKUP_BUCKET/backups/` (kept 30 days), together with the key file
`data/secret.key` at `s3://BACKUP_BUCKET/keys/secret.key` (not expired).
That key signs the unsubscribe, click, open and confirmation links in every
email already sent: a new server with a new key would break those links.
Uploaded images (`data/media/`) are not in the backup. Hetzner's server
backups (enabled in AGENTS.md phase 2) cover the whole disk, including
`data/`.

To keep a copy off the server as well, copy `data/secret.key`,
`data/media/` and the server's `.env` somewhere safe (a password manager
or an encrypted drive: `.env` holds the sending user's secret key) after
the first start and after uploading images, e.g.
`sudo tar -C /opt/mailroom -czf mailroom-files.tgz .env data/secret.key data/media`.

### What a restore does

A backup is up to a day old, and restoring it would otherwise undo
everything since: unsubscribes, bounces, suppressions, GDPR erasures, and
which queued emails were already sent. So `mailroom restore`:

- **pauses all sending** in the restored database (a manual pause, with a
  reason starting "restored from a backup"). Nothing is sent after the start
  until someone resumes it: `POST /api/v1/sending/resume`, or "Resume
  sending" on the Health page of the admin UI;
- **carries opt-outs and sends over** from the database it replaces (its
  safety copy, `data/backups/pre-restore-STAMP.db`): suppressions; contacts
  erased there are erased again; unsubscribed, bounced, complained and
  suppressed statuses (and unsubscribe requests); contacts created after
  the backup that opted out; messages that were queued in the backup but
  already sent, and broadcast and sequence emails sent after it; how
  far each sequence enrollment got, and every "only stop these emails"
  opt-out from a sequence. It never goes the other way: a
  resubscribe, a lifted suppression, new contacts, tags or content changes
  after the backup are not carried over. The output (and `--json`) lists
  the counts and every `WARNING`;
- **never brings back access that was taken away**: API tokens revoked
  (or gone) in the replaced database are revoked, users deleted there are
  deleted, users keep the password and two-factor settings they had there,
  and every admin UI session ends, so everyone signs in again. Tokens and
  users created after the backup are gone: create them again if needed.

When there was no database to replace, as on a new server, nothing can be
carried over, and the output says so: every opt-out and erasure after the
backup is lost, and emails queued in the backup may already have gone out.
Tokens revoked, users deleted and passwords changed after the backup are
back too: check the Tokens and Users pages and revoke or reset them.
Before resuming, re-apply the opt-outs you know of (SES's account-level
suppression list keeps bounces and complaints, but not unsubscribes or
erasures), and check the queued broadcasts and sequences. The same applies
when you go back to an older binary by restoring a `pre-migrate-…` backup.

**Rebuilding on a new server:**

1. Set it up as in steps 1 to 3, but don't start it yet. In step 2, put
   your copy of the old server's `.env` in place (owner 65532, mode 600)
   instead of writing a new one: it has the sending keys, `SNS_TOPIC_ARN`
   and `BACKUP_BUCKET`, so nothing in AWS has to change.
2. **Without a copy of the old `.env`:** write it as in step 2, then run
   `provision ses --rotate-keys` as in step 4. It needs a setup key: in the
   AWS console (IAM → Users → `setup-agent` → Security credentials),
   **create a new access key** for this run and delete it afterwards. Don't
   switch the old, deactivated key back on: a copy of it may survive in old
   logs, shell histories or backups, and switching it on makes that copy
   valid again. It finds the existing identity, topic, bucket and sending user, and
   writes a new sending key, `SNS_TOPIC_ARN` and `BACKUP_BUCKET` into
   `.env`. Without `--rotate-keys` it stops, because the sending user
   already has a key that isn't in the new `.env`.
3. Put the key file and the latest backup in place, and restore:

   ```bash
   cd /opt/mailroom
   aws s3 cp s3://BACKUP_BUCKET/keys/secret.key /tmp/secret.key          # or from your own copy
   aws s3 cp s3://BACKUP_BUCKET/backups/mailroom-STAMP.db /tmp/mailroom-STAMP.db
   sudo install -m 600 -o 65532 -g 65532 /tmp/secret.key data/secret.key
   sudo install -d -m 0700 -o 65532 -g 65532 data/backups
   sudo install -m 600 -o 65532 -g 65532 /tmp/mailroom-STAMP.db data/backups/
   sudo docker compose run --rm mailroom restore /data/backups/mailroom-STAMP.db
   ```

4. If the new server has a new IP address, change the A (and AAAA) record
   of `APP_SUBDOMAIN.DOMAIN` to it at your DNS provider **before** starting:
   the HTTPS certificate and the SES webhook need the name to reach the new
   server. `provision dns` never changes an existing record (it reports the
   old one as a conflict), so change it by hand, and check it with
   `sudo docker compose run --rm mailroom doctor` (`dns_app`).
5. Start it: `sudo docker compose up -d`, then check `doctor` as in step 6.
   Sending stays paused (see [What a restore does](#what-a-restore-does)):
   re-apply the opt-outs you know of since the backup, check the queued
   broadcasts and sequences, then resume sending.

Restore `data/media/` from your copy (owned by 65532) if you have one. The
`aws` commands (AWS CLI) need credentials that may read objects in the
bucket (`s3:GetObject`): the sending user can only write to it, and the
setup user from the guide (step 2.6) can only configure it. The simplest
way is to download the two files in the S3 console, signed in to the AWS
account, and copy them to the server's `/tmp/`.

## Behind a reverse proxy

If another proxy already terminates TLS on the server, turn autocert off.
Set these in `.env`:

```bash
HTTP_ONLY=1
HTTP_ADDR=:8080
TRUSTED_PROXIES=172.16.0.0/12   # the proxy's address as the container sees it
```

In `docker-compose.yml`, replace the two port lines with
`- "127.0.0.1:8080:8080"`, and point the proxy at `http://127.0.0.1:8080`.
Set `HTTP_ONLY` in `.env`, not with `serve --http-only`: the health check
reads `.env` to find the port.

## Running without sudo

The container user must be able to write `data/` and `.env`, and the
`docker compose` command must be able to read `.env`. If the operator's
account is in the `docker` group and you want to drop `sudo`, run the
container as that account instead of uid 65532:

```bash
cd /opt/mailroom
echo "MAILROOM_UID=$(id -u)" | sudo tee -a .env
echo "MAILROOM_GID=$(id -g)" | sudo tee -a .env
sudo chown -R "$(id -u):$(id -g)" .env data
sudo chmod 600 .env
docker compose up -d
```

Compose reads `MAILROOM_UID` and `MAILROOM_GID` from `.env` for the
`user:` line of the compose file.
