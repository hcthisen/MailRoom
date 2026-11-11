# Mailroom

Mailroom is a self-hosted email marketing app by **Scale By Tech**. Manage contacts, send newsletters, build drip sequences, collect sign-ups, and handle replies through a web interface or REST API. Email delivery runs through Amazon SES.

## Features

- **Contacts and segments:** tags, custom attributes, saved audience filters, consent history, and CSV imports.
- **Newsletters:** draft, preview, test, schedule, and send broadcasts to your contacts or a segment.
- **Drip sequences:** timed email steps with form or tag triggers and exit tags.
- **Sign-up forms:** hosted forms and website embeds with optional double opt-in.
- **Email content:** Markdown or HTML, merge fields, reusable layouts, and media uploads.
- **Delivery handling:** one-click unsubscribe, bounce and complaint processing, and contact suppression.
- **Inbox:** receive email and reply from Mailroom when SES receiving is configured.
- **Statistics:** delivery results, clicks, contact timelines, and dashboard summaries.
- **Administration:** API tokens, two-factor authentication, audit logs, health checks, and local or S3 backups.

## How it runs

Mailroom uses one Go binary, one SQLite database, and one Docker container. Each installation sends from one domain.

```text
Website forms → Mailroom → Amazon SES → Recipients
                   ↑           │
                   └── SNS ────┘
                  Delivery events
```

The admin interface is at `/admin/`, the REST API at `/api/v1/`, and the API guide at `/api/v1/docs`.

## Installation

For production, configure your sending domain, Amazon SES, and a server using either deployment option:

| Option | Setup |
|---|---|
| **VPS** | Run Mailroom with Docker Compose and built-in HTTPS. Follow the [VPS deployment guide](deploy/README.md). |
| **Coolify** | Deploy the root [docker-compose.yaml](docker-compose.yaml), with HTTPS handled by Coolify. Follow the [Coolify deployment guide](deploy/coolify.md). |

For guided setup, open this repository in a coding agent and type `start`. The agent follows [AGENTS.md](AGENTS.md) and the [setup guide](guide/steps.md) to configure accounts, deployment, and email delivery. Importing contacts and workflows from GoHighLevel is optional.

## Local development

Requires Go 1.26 or newer. Dependencies are included in `app/vendor/`.

From the repository root:

```bash
cd app
go build -o mailroom ./cmd/mailroom

export DOMAIN=example.com
export MAILROOM_DATA_DIR=./data
export HTTP_ONLY=1
export BASE_URL=http://localhost:8080

./mailroom serve
```

In a second terminal, use the same `app/` directory and environment to create an admin account:

```bash
./mailroom admin create --email owner@example.com
```

Open [localhost:8080](http://localhost:8080) and sign in with the generated credentials. Configure SES credentials to enable email delivery.

Run the tests from `app/`:

```bash
bash scripts/test.sh
```

See the [developer guide](app/README.md) for build options, configuration, and testing details.

## Documentation

| Document | Contents |
|---|---|
| [Setup guide](guide/steps.md) | Guided account and infrastructure setup |
| [VPS deployment](deploy/README.md) | Docker deployment, HTTPS, backups, and upgrades |
| [Coolify deployment](deploy/coolify.md) | Deployment on an existing Coolify server |
| [API guide](app/docs/api.md) | Authentication and API operations |
| [OpenAPI specification](app/api/openapi.yaml) | API endpoints and schemas |
| [Contact import format](app/docs/import-format.md) | CSV columns and import behavior |
| [Sequence format](app/docs/sequence-format.md) | YAML format for drip sequences |
| [Developer guide](app/README.md) | Building, testing, and extending Mailroom |
| [Architecture](app/ARCHITECTURE.md) | Packages, data model, and runtime design |

## License

Mailroom is released under the [MIT License](LICENSE).
