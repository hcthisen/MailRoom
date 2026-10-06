# Leaving GoHighLevel: your new email setup

**{{AGENCY}}** is closing its GoHighLevel agency account on **{{SHUTDOWN_DATE}}**. Your sub-account closes at the same time, and everything in it is deleted: contacts, tags, workflows and emails. It cannot be restored.

This kit gives you the cheapest way to move on, with a self-hosted setup that you own:

| Part | What it does | Price (approx.) |
|---|---|---|
| **Mailroom** on a small VPS (or your Coolify server) | One app for contacts, tags, sign-up forms, drip sequences, newsletters, unsubscribes and statistics. Everything in it can also be controlled through its API | VPS approx. €5–7/month incl. backups |
| **Amazon SES** | The actual delivery to Gmail, Outlook etc. | approx. $0.10 per 1,000 emails |

Mailroom is open source (MIT licence) and included in this kit as source code. It sends from **one domain**: yours.

**You only create accounts and API keys.** A coding agent (Claude Code or Codex) does the technical work by following [AGENTS.md](AGENTS.md): it exports your GoHighLevel data, creates the server, builds and starts Mailroom, sets up Amazon SES and your DNS records, and moves your contacts, drip sequences and forms. It stops and asks for your "yes" at five checkpoints. After the setup, you or your agent can manage everything in Mailroom through its web interface (e.g. `https://newsletter.yourdomain.com`) or its API.

New Amazon SES accounts start in a test mode (the "sandbox") that can only send to your own domain. The agent asks AWS to lift it for you. AWS usually answers by email within a day, and nothing goes to your contacts until then.

## Start here

1. Install a coding agent: [Claude Code](https://claude.com/claude-code) or [Codex](https://openai.com/codex).
2. Open a terminal in this folder and start it (`claude` or `codex`).
3. Type **`start`**.

The agent creates your settings file, opens it in Notepad or TextEdit, and guides you through every step: which accounts to create, where to click, and where to paste each key. Then it does the technical work and stops for your "yes" at five checkpoints. It reads its instructions from [AGENTS.md](AGENTS.md) (Claude Code finds them through [CLAUDE.md](CLAUDE.md)). The same steps are written out in [guide/steps.md](guide/steps.md) if you want to read ahead.

When the setup is finished, the agent marks it as done in `AGENTS.md`. From then on, starting the agent here gives you a helper for your newsletters and drip emails ("write this month's newsletter", "change the second welcome email"). It won't redo the setup.

## Two ways to run Mailroom

| | Own server (default) | Coolify |
|---|---|---|
| For | A new Hetzner server the agent creates through the Hetzner API (or a server you already have) | A server that already runs [Coolify](https://coolify.io), possibly with other services |
| HTTPS | Mailroom gets its own certificate (ports 80/443) | Coolify's proxy handles HTTPS; Mailroom publishes no ports, so nothing clashes |
| How | [deploy/README.md](deploy/README.md), done by the agent | Deploy this repository with the root [docker-compose.yaml](docker-compose.yaml): [deploy/coolify.md](deploy/coolify.md), by the agent through Coolify's API or by hand in Coolify |

Choose with `DEPLOY_TARGET` in `.env`.

## Timeline

| When | What |
|---|---|
| **Now** | Part 1: Export everything from GoHighLevel |
| At least 3 weeks before {{SHUTDOWN_DATE}} | Parts 2–6: Accounts, keys, and start the agent |
| At least 1 week before {{SHUTDOWN_DATE}} | New website forms are live and the new drip sequences are running |
| {{SHUTDOWN_DATE}} | GoHighLevel closes |

## What's in the kit

| File | For |
|---|---|
| [guide/steps.md](guide/steps.md) | The agent's script for guiding you (you can read ahead): what to click, step by step. The screenshots (📸) are still to come: [guide/CAPTURE.md](guide/CAPTURE.md) is the brief for capturing them, and until they are added the text alone describes every step |
| `.env.example` → `.env` | You: where your keys and settings go. **Never share `.env`.** |
| `ghl-export/` | You: your exported GoHighLevel data and workflow notes |
| [AGENTS.md](AGENTS.md) | The coding agent: the technical runbook (`CLAUDE.md` points Claude Code to it) |
| [app/](app/) | Mailroom's source code and documentation ([PRD](app/PRD.md), [API spec](app/api/openapi.yaml), [import and sequence formats](app/docs/)) |
| [deploy/](deploy/) | How Mailroom runs on its own server: the Docker Compose file and its [README](deploy/README.md) (install, backups, upgrades), and [coolify.md](deploy/coolify.md) for Coolify |
| [docker-compose.yaml](docker-compose.yaml) | The Coolify deployment (Build Pack "Docker Compose") |
| [notes/](notes/) | The original planning conversation |
| [LICENSE](LICENSE) | MIT licence for the whole kit |

## Responsibility

This kit and Mailroom are provided **as-is** (see [LICENSE](LICENSE)). They are one-off transition tools, not a product or service. {{AGENCY}} does not provide setup, operation, updates or support, and accepts no liability. You own your installation. You are the data controller for your contacts, and you are responsible for making sure your emails comply with marketing law and GDPR.

## Legal in brief (not legal advice)

- You may only send marketing to people who have given consent (in Denmark: section 10 of the Marketing Practices Act). Keep records of where and when they signed up. The GHL export is your documentation for the existing contacts.
- Every marketing email, including drip emails, has a one-click unsubscribe link. Mailroom adds it automatically.
- Mailroom records which links each contact clicks. The default consent text on your forms says so; keep it in your privacy policy too.
- Contacts who unsubscribed in GHL are imported as **unsubscribed** and will never receive marketing email again.
- AWS and Hetzner process data on your behalf. AWS's data processing addendum (DPA) is automatically part of their terms. With Hetzner you need to conclude the data processing agreement (AVV) yourself (step 3.4).
