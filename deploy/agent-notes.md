# Deployment lessons for agents

Read this before provisioning, alongside [the setup checkpoints](../AGENTS.md)
and [the VPS commands](README.md) or [Coolify guide](coolify.md). This is
reference guidance; it does not authorize infrastructure changes or restart
a completed installation's setup. Read its `PROGRESS.md` and operations notes
first. Record deviations there without secrets.

## Resolve the deployment contract first

Record the sending `DOMAIN`, From and Reply-To addresses, owner/test address,
company postal footer, AWS account/region, deployment target, public app URL,
DNS zone, receiving plan, and migration scope once. Carry forward approvals
already given for those exact actions; ask only for missing decisions or a
materially changed plan. Keep approval boundaries for DNS, imports and sending
separate. A contact import is not permission to enroll or email those contacts.

Read account state before choosing a region: SES production access, identity
and receiving support are region-specific. An explicit organization-policy
deny is not fixed by adding more IAM grants. Explain the restriction and
review an allowed region with the owner; do not change account billing or
organization policies as a workaround.

Use parallel read-only discovery where possible: source/migration inventory,
AWS/DNS inventory, and server readiness are independent. Building the vendored
VPS image can overlap an approved AWS setup; it needs no local Go toolchain.
Give each sub-agent a bounded output and separate files. One coordinator owns
the resource plan, secret files, authorization and checkpoint evidence, and
coordinates independent workers' live changes. Serialize work in the same
browser tab and changes to the same cloud resource. Separate owned tabs can
support independent work where the browser tool permits it. Never
let two agents provision, rotate, bootstrap or cut over MX concurrently.

## Inventory AWS before `provision ses`

The default CLI is a reconciler, not a harmless create-only command. Its
resource names are partly fixed; it has **no installation namespace flag**.
An existing installation in the same AWS account can be affected, even in a
different region because IAM is account-wide. Inspect IAM users/policies/keys,
SES identities/configuration destinations, SNS topics/policies/subscriptions,
S3 buckets and the active receipt rule set before any provisioning command.

| Resource | Default name or collision scope |
|---|---|
| IAM sender user and inline policy | `mailroom-sender` (account-wide) |
| SES configuration set | `mailroom` (environment override supported) |
| Configuration event destination | `mailroom-sns` within the set |
| SNS topic | `mailroom-ses-events` in the region |
| Backup/inbound buckets | `mailroom-backup-ACCOUNT-REGION`, `mailroom-inbound-ACCOUNT-REGION` |
| Receipt rule set / rule | `mailroom` / `mailroom-inbound` in the region |

Reconciliation can replace the sender's inline policy, rotate/delete its
keys, replace the SNS topic policy, update the event destination, update the
receipt rule and activate a rule set. Setting only `SES_CONFIGURATION_SET`
does **not** isolate the remaining names. Do not apply the default provisioning
or `--rotate-keys` recovery recipe to an unrelated or shared installation.
Source: [names](../app/internal/provision/names.go),
[CLI flags](../app/internal/provision/cli_ses.go),
[IAM](../app/internal/provision/iam.go), [SNS](../app/internal/provision/sns.go),
[SES](../app/internal/provision/ses.go), [receiving](../app/internal/provision/inbound.go).

The simplest isolation is a dedicated AWS account. If the owner chooses a
shared account, prepare an explicit reviewed provisioning plan outside the
default CLI, using AWS APIs/CLI and installation-specific names such as
`mailroom-example-sender`, `mailroom-example`, and `mailroom-example-ses-events`.
Include both buckets, policies, event destination, subscriptions and receipt
rule. Do not imply that these names are accepted CLI flags.

For that plan:

- Capture non-secret before/after invariants for existing installations:
  policy documents, active rule set, rule order/content, configuration
  destinations, topic policy/subscriptions and sender key identifiers.
- Use an exact resource-name allowlist and a creation ledger. Fail on an
  unexpected existing resource instead of adopting it by name. Reconcile an
  uncertain mutation with read calls before retrying it.
- If the setup policy permits only default names, show the exact additional
  actions/resource ARNs and get approval for that narrower addition. Do not
  replace it with unrestricted administrator access.
- Restrict sending to the chosen identity; scope SNS publish to the source
  account and exact configuration-set/receipt-rule ARNs, and subscribe only
  the intended HTTPS webhook. Scope inbound S3 writes to the rule and prefix;
  scope the sender's inbound reads/deletes and backup access to its own data.
- Store only this installation's runtime keys, topic, configuration set and
  bucket values in its server `.env`. Document that future default provisioning
  and key-rotation recipes must not be used for this installation.

SES permits only one active receipt rule set per region. With an explicit
receiving plan, adding a distinct rule to the existing active set may be
appropriate **only** after verifying disjoint recipient conditions and
preserving every existing rule and its order. Never add a catch-all, replace
another tenant's rule, or activate another set. Check stop actions and rule
overlap before claiming isolation. See [AWS receipt rule behavior](https://docs.aws.amazon.com/ses/latest/dg/receiving-email-receipt-rules-console-walkthrough.html).

After creating a new IAM key, an immediate `InvalidClientTokenId` can be
propagation delay. Persist its one-time secret privately once; use bounded
backoff and read-only STS calls with the same key. Check account, key status
and credentials if it persists. Do not create or rotate keys repeatedly to
solve propagation. See [IAM eventual consistency](https://docs.aws.amazon.com/IAM/latest/UserGuide/troubleshoot.html#troubleshoot_general_eventual-consistency).

## Separate sending identity, web host and DNS zone

For example, these are valid separate settings:

```dotenv
DOMAIN=hi.example.com
FROM_EMAIL=news@hi.example.com
REPLY_TO_EMAIL=hello@hi.example.com
BASE_URL=https://mailroom.example.com
```

From and Reply-To must both use `DOMAIN`; the owner alert address may be
elsewhere. `BASE_URL` controls the public app host, TLS and webhook URL. Set it
before first start and ensure the proxy, if any, serves that host.

**DNS CLI limitation:** app A/AAAA records still concatenate
`APP_SUBDOMAIN.DOMAIN`; `BASE_URL` does not change that generator. In this
example `APP_SUBDOMAIN=mailroom` would generate `mailroom.hi.example.com`.
Generate SES records without `--ipv4`/`--ipv6`, then include the actual app
host's A/AAAA in a separate reviewed DNS plan. Check the public host with
doctor; don't publish an unused hostname just to satisfy the printed plan.
Source: [config](../app/internal/config/config.go),
[record generator](../app/internal/provision/records.go),
[DNS checks](../app/internal/system/checks_dns.go).

`DOMAIN=hi.example.com` may belong to the Cloudflare zone `example.com`.
A preflight lookup of only `zones?name=hi.example.com` can return nothing.
Find the closest owning parent zone and scope its DNS token to that zone.
The built-in [Cloudflare client](../app/internal/provision/cloudflare.go)
already searches parent zones. Use fully qualified record names; don't append
the zone twice.

## Stage DNS and the incoming-mail cutover

Export the current affected record IDs/values and explain each deletion or
replacement. Root-domain Google Workspace/Microsoft 365 mail need not move
when a separate sending subdomain moves. State exactly which domain's incoming
mail will reach Mailroom; all addresses on that domain follow its MX.

1. Prepare the app host, DKIM and MAIL FROM records. Validate sending identity,
   HTTPS, SNS delivery, inbound storage/policies and the intended receipt rule.
2. Recheck current provider records immediately before an approved change.
   Then perform the incoming MX cutover only when its destination is ready.
3. Verify authoritative and public DNS, then an external inbound test. Check
   unrelated mailbox routing and the existing installation's invariants.

`provision dns` creates records only; it does not resolve migration conflicts
by editing or deleting them. Use a separate reviewed provider/API change for
those records. Keep old Mailgun tracking CNAMEs during transition unless the
owner approves removal knowing that historical click and unsubscribe links
may break. The old links can matter after new sending has moved.

An early lookup can cache NXDOMAIN after correct records are published.
Compare the provider, authoritative answer, public resolver and local resolver
before changing DNS again. For a TLS-preserving server diagnostic use:

```bash
curl --resolve mailroom.example.com:443:203.0.113.10 https://mailroom.example.com/healthz
```

This checks the intended server with normal hostname/certificate verification;
it does not prove public DNS readiness. Never disable TLS verification or
change security settings to make a test pass. Use only permitted OS/browser
cache tools; on macOS a permitted `dscacheutil -flushcache` can clear an OS
cache, while a browser may still cache independently. If browser policy blocks
an internal cache page, respect it: wait for expiry or ask the owner to check
normally. Confirm actual sign-in through the public URL before handover.

## Server bootstrap without lockout or secret leakage

Inspect the existing OS/resources before replacing anything. Follow the
[current official Docker Ubuntu instructions](https://docs.docker.com/engine/install/ubuntu/)
for that OS/codename, supported architecture and repository. Do not reinstall
a supported OS or substitute another release's package suite by assumption.
Docker-published ports can bypass UFW; publish only the intended ports.

Generate the SSH key locally in `secrets/`. From the initial authorized login,
create the non-root user, install the public key and validate the sudoers file.
In a separate key-based SSH session, prove `sudo -n true` works. Only then
disable root/password SSH, validate effective sshd settings, reload and test
another fresh session. Preserve the working session until that succeeds.

Create the server `.env` before any Compose command; give it uid 65532 and mode
600, with `data/` mode 700. Copy a runtime-field allowlist rather than the local
setup `.env`. Setup AWS/Cloudflare/Hetzner/GHL credentials stay off the server
disk. Pass short-lived setup values over SSH stdin and environment names only,
as in [README step 4](README.md#4-provision-ses), never `sudo env KEY=value` or
`docker -e KEY=value` arguments. Avoid shell tracing, verbose authenticated
HTTP logging and printing environment/config files.

Bootstrap outputs are one-time secrets: redirect admin/token stdout directly
to private local `secrets/` files (directory 700, files 600). Reconcile an
uncertain command result before creating another admin or token. Give the
owner the login URL/email and credential **file path**, not the password in
chat. Record whether a future-assistance API token is retained or revoked.

## Verify the running API and make recovery complete

### Portable local tooling

- Check installed tool versions before writing orchestration. macOS may ship
  an older Tcl/Expect without newer commands such as `binary encode`; use
  available standard tools for non-secret encoding instead. When initial
  password-based SSH needs Expect, read the password privately and disable
  transcript logging; never pass it as a process argument.
- If an AWS CLI version rejects `--cli-input-json file:///dev/stdin`, use a
  mode-600 request file under `secrets/`. Capture credential-creation output
  straight into a private file, never into the tool transcript.
- Avoid shell variables named `path`, `PATH`, `HOME` or `home`; in zsh `path`
  is tied to command lookup. Use descriptive task-specific names.

### API and recovery checks

Use the running `/api/v1/docs` and `/api/v1/openapi.json` for the exact method,
body and response; compare source if behavior differs. For example email
preview is `POST /api/v1/emails/{id}/preview`, not GET. Treat HTTP header names
case-insensitively when extracting `ETag`. Supply POST idempotency keys and
PATCH/PUT preconditions as documented; after fixing a rejected request, use
a new key. A dry run may return an immediate report while the actual action
returns a job: follow that endpoint's schema and poll to completion.

Read API tokens from a private file into the HTTP client's memory; avoid
putting bearer headers in process arguments, logs or chat. Save reports
without tokens or contact payloads in public repository documentation.

A database backup alone is not a full recovery kit. Preserve privately:

- the final database backup and successful off-server backup result;
- the original `data/secret.key` signing key, or the original explicit
  `MAILROOM_SECRET` if configured;
- the exact server runtime `.env`, including this installation's resource
  names and sending credentials;
- uploaded `data/media/` and the deployed source version.

`data/media/` may not exist before the first upload; handle its absence rather
than treating it as a failed installation. Refresh its archive after adding
logos/images. Run a final database backup **after** imports, settings, layout
and forms are complete, and verify `s3_key` and `secret_key_s3_key` when
applicable. Verify private recovery file permissions and integrity without
displaying their contents. See [backup/restore](README.md#backups-and-disaster-recovery).

Restores pause sending. On an empty replacement server there is no current
database from which to carry forward newer opt-outs or revocations; reconcile
them before resuming. The sender's restricted S3 access does not grant general
backup download access: use the authenticated backup API or an authorized
backup-reader path. Preserve the original signing key so old unsubscribe,
tracking and confirmation links continue to work.
