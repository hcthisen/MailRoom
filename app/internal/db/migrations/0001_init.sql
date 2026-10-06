-- Mailroom schema v1. SQLite. All times are TEXT in UTC, format "2006-01-02T15:04:05.000000Z"
-- (fixed width, so they sort and compare lexicographically). JSON columns are TEXT with json_valid().
-- Mutable API resources carry a `version` column used for ETag / If-Match.

-- ---------------------------------------------------------------- settings / auth / system

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL CHECK (json_valid(value)),
  updated_at TEXT NOT NULL
);

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name          TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,                 -- argon2id, PHC string format
  totp_secret   TEXT,                          -- base32; NULL = TOTP off
  totp_last_step INTEGER NOT NULL DEFAULT 0,   -- last accepted RFC 6238 time step (replay protection)
  version       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  last_login_at TEXT
);

CREATE TABLE sessions (
  id           TEXT PRIMARY KEY,               -- sha256 hex of the cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token   TEXT NOT NULL,
  ip           TEXT NOT NULL DEFAULT '',
  user_agent   TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  expires_at   TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
  id                 INTEGER PRIMARY KEY,
  name               TEXT NOT NULL UNIQUE,
  token_hash         TEXT NOT NULL UNIQUE,     -- sha256 hex of the full token
  prefix             TEXT NOT NULL,            -- first 12 chars, shown in lists ("mr_ab12cd34…")
  scope              TEXT NOT NULL CHECK (scope IN ('admin','read')),
  created_by         TEXT NOT NULL DEFAULT '', -- actor string, e.g. "user:1", "cli"
  created_at         TEXT NOT NULL,
  expires_at         TEXT,
  last_used_at       TEXT,
  revoked_at         TEXT
);

CREATE TABLE audit_log (
  id          INTEGER PRIMARY KEY,
  actor_type  TEXT NOT NULL CHECK (actor_type IN ('user','token','system','public','cli')),
  actor_id    TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',
  operation   TEXT NOT NULL,                   -- e.g. "contact.update", "broadcast.send"
  target_type TEXT NOT NULL DEFAULT '',
  target_id   TEXT NOT NULL DEFAULT '',
  changes     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(changes)),
  created_at  TEXT NOT NULL
);
CREATE INDEX audit_created ON audit_log(created_at);
CREATE INDEX audit_target ON audit_log(target_type, target_id);

CREATE TABLE idempotency_keys (
  actor         TEXT NOT NULL,                 -- "token:<id>" or "user:<id>"
  key           TEXT NOT NULL,
  method        TEXT NOT NULL,
  path          TEXT NOT NULL,
  request_hash  TEXT NOT NULL,                 -- sha256 of method+path+body
  status_code   INTEGER NOT NULL,
  response_body BLOB NOT NULL,
  response_type TEXT NOT NULL DEFAULT 'application/json',
  created_at    TEXT NOT NULL,
  PRIMARY KEY (actor, key)
);
CREATE INDEX idempotency_created ON idempotency_keys(created_at);

CREATE TABLE jobs (
  id          INTEGER PRIMARY KEY,
  type        TEXT NOT NULL,                   -- "import.contacts", "broadcast.snapshot", "backup", "tags.bulk", ...
  status      TEXT NOT NULL CHECK (status IN ('queued','running','succeeded','failed')),
  progress    REAL NOT NULL DEFAULT 0,         -- 0..1
  params      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(params)),
  result      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(result)),
  error       TEXT NOT NULL DEFAULT '',
  created_by  TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',        -- display name of created_by (user email / token name)
  created_at  TEXT NOT NULL,
  started_at  TEXT,
  finished_at TEXT
);
CREATE INDEX jobs_status ON jobs(status);

CREATE TABLE media (
  id           INTEGER PRIMARY KEY,
  filename     TEXT NOT NULL,                  -- original name, for display
  stored_name  TEXT NOT NULL UNIQUE,           -- random name on disk under <data>/media/
  content_type TEXT NOT NULL,
  size_bytes   INTEGER NOT NULL,
  created_at   TEXT NOT NULL
);

-- ---------------------------------------------------------------- contacts

-- AUTOINCREMENT: the IDs of erased contacts are never reused, because signed
-- public tokens (unsubscribe, click, open, opt-in) carry contact IDs.
CREATE TABLE contacts (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  email             TEXT NOT NULL UNIQUE,       -- normalised: trimmed + lowercased
  first_name        TEXT NOT NULL DEFAULT '',
  last_name         TEXT NOT NULL DEFAULT '',
  attributes        TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attributes)),
  status            TEXT NOT NULL CHECK (status IN ('pending','active','unsubscribed','bounced','complained','suppressed_manual')),
  status_changed_at TEXT NOT NULL,
  -- An unsubscribe (link or one-click) that arrived while the status could not
  -- become unsubscribed (bounced, complained, suppressed_manual). A later
  -- reactivation lands on unsubscribed instead of active (U-3). NULL = none.
  unsubscribe_requested_at TEXT,
  version           INTEGER NOT NULL DEFAULT 1,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);
CREATE INDEX contacts_status ON contacts(status);
CREATE INDEX contacts_created ON contacts(created_at);

CREATE TABLE tags (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
  created_at TEXT NOT NULL
);

CREATE TABLE contact_tags (
  contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
  tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  source     TEXT NOT NULL DEFAULT '',
  added_at   TEXT NOT NULL,
  PRIMARY KEY (contact_id, tag_id)
) WITHOUT ROWID;
CREATE INDEX contact_tags_tag ON contact_tags(tag_id, contact_id);

-- Append-only consent log (C-3). Deleted only by GDPR erase (cascade).
CREATE TABLE consent_events (
  id           INTEGER PRIMARY KEY,
  contact_id   INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
  type         TEXT NOT NULL CHECK (type IN ('signup','optin_requested','optin_confirmed','import','admin','api','resubscribe')),
  source       TEXT NOT NULL,                  -- "form:<public_id>", "import:<consent_source>", "api:<token name>", "admin:<user email>"
  form_id      INTEGER,                        -- no FK: the log must survive form deletion
  ip           TEXT NOT NULL DEFAULT '',
  user_agent   TEXT NOT NULL DEFAULT '',
  consent_text TEXT NOT NULL DEFAULT '',
  occurred_at  TEXT NOT NULL,                  -- when consent was given (import: consent_date)
  created_at   TEXT NOT NULL
);
CREATE INDEX consent_contact ON consent_events(contact_id);

-- Every status transition (U-4, timeline). Deleted only by GDPR erase (cascade).
CREATE TABLE contact_status_changes (
  id          INTEGER PRIMARY KEY,
  contact_id  INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
  from_status TEXT NOT NULL,
  to_status   TEXT NOT NULL,
  source      TEXT NOT NULL,                   -- link | one_click | admin | api | import | bounce | complaint | form | optin | system
  message_id  INTEGER REFERENCES messages(id) ON DELETE SET NULL,
  actor       TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE INDEX status_changes_contact ON contact_status_changes(contact_id);

-- Addresses that must never be (re-)added or emailed, independent of a contact row.
-- 'erased' rows are written by GDPR erase (C-6); 'manual' rows by the suppressions API.
CREATE TABLE suppressions (
  email_hash TEXT PRIMARY KEY,                 -- sha256 hex of the normalised email
  reason     TEXT NOT NULL CHECK (reason IN ('erased','manual')),
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE segments (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  filter     TEXT NOT NULL CHECK (json_valid(filter)),
  version    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- ---------------------------------------------------------------- content

CREATE TABLE emails (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  subject    TEXT NOT NULL,
  preheader  TEXT NOT NULL DEFAULT '',
  format     TEXT NOT NULL CHECK (format IN ('markdown','html')),
  body       TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- ---------------------------------------------------------------- sequences

CREATE TABLE sequences (
  id              INTEGER PRIMARY KEY,
  name            TEXT NOT NULL UNIQUE,
  status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
  trigger_tag_id  INTEGER REFERENCES tags(id) ON DELETE SET NULL,   -- NULL = no tag trigger
  exit_tag_ids    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(exit_tag_ids)),
  exit_add_tag_id INTEGER REFERENCES tags(id) ON DELETE SET NULL,
  reentry         TEXT NOT NULL DEFAULT 'never' CHECK (reentry IN ('never','after_completion','always')),
  send_window     TEXT CHECK (send_window IS NULL OR json_valid(send_window)), -- {"days":[1,2,3,4,5],"start":"08:00","end":"20:00"}
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE sequence_steps (
  id            INTEGER PRIMARY KEY,
  sequence_id   INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,              -- 1-based; reorder via a two-step update
  delay_seconds INTEGER NOT NULL CHECK (delay_seconds >= 0),
  email_id      INTEGER NOT NULL REFERENCES emails(id),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE (sequence_id, position)
);

CREATE TABLE enrollments (
  id           INTEGER PRIMARY KEY,
  sequence_id  INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
  contact_id   INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
  next_step_id INTEGER REFERENCES sequence_steps(id) ON DELETE SET NULL,  -- step to send next
  next_run_at  TEXT,                           -- NULL when not active
  status       TEXT NOT NULL CHECK (status IN ('active','completed','exited')),
  exit_reason  TEXT NOT NULL DEFAULT '',       -- not_active | exit_tag | manual | sequence_deleted | step_deleted | unsubscribed_sequence
  source       TEXT NOT NULL DEFAULT '',       -- form:<id> | tag:<name> | api | admin | migration
  started_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  finished_at  TEXT
);
CREATE INDEX enrollments_due ON enrollments(next_run_at) WHERE status = 'active';
CREATE INDEX enrollments_contact ON enrollments(contact_id);
CREATE UNIQUE INDEX enrollments_one_active ON enrollments(sequence_id, contact_id) WHERE status = 'active';

-- ---------------------------------------------------------------- forms

CREATE TABLE forms (
  id              INTEGER PRIMARY KEY,
  public_id       TEXT NOT NULL UNIQUE,        -- random slug used in /f/{public_id}
  name            TEXT NOT NULL,
  fields          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(fields)),   -- [{"key":"first_name","label":"First name","required":false}, {"key":"attr.company",...}]
  tag_ids         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tag_ids)),
  sequence_id     INTEGER REFERENCES sequences(id) ON DELETE SET NULL,
  double_optin    INTEGER NOT NULL DEFAULT 0 CHECK (double_optin IN (0,1)),
  consent_text    TEXT NOT NULL,
  redirect_url    TEXT NOT NULL DEFAULT '',
  allowed_origins TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(allowed_origins)),
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE form_submissions (
  id         INTEGER PRIMARY KEY,
  form_id    INTEGER NOT NULL REFERENCES forms(id) ON DELETE CASCADE,
  contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  outcome    TEXT NOT NULL CHECK (outcome IN ('created','updated','optin_pending','resubscribe_pending','rejected','spam','rate_limited')),
  created_at TEXT NOT NULL
);
CREATE INDEX form_submissions_form ON form_submissions(form_id, created_at);

CREATE TABLE pending_optins (
  id           INTEGER PRIMARY KEY,
  form_id      INTEGER REFERENCES forms(id) ON DELETE SET NULL,
  contact_id   INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL CHECK (kind IN ('optin','resubscribe')),
  payload      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),  -- fields/tags/sequence to apply on confirm
  token_hash   TEXT NOT NULL UNIQUE,
  ip           TEXT NOT NULL DEFAULT '',
  user_agent   TEXT NOT NULL DEFAULT '',
  consent_text TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  confirmed_at TEXT
);
CREATE INDEX pending_optins_expires ON pending_optins(expires_at) WHERE confirmed_at IS NULL;

-- ---------------------------------------------------------------- broadcasts

CREATE TABLE broadcasts (
  id               INTEGER PRIMARY KEY,
  name             TEXT NOT NULL,
  email_id         INTEGER NOT NULL REFERENCES emails(id),
  audience         TEXT NOT NULL DEFAULT '{"type":"all"}' CHECK (json_valid(audience)), -- {"type":"all"} | {"type":"segment","segment_id":3,"segment_name":"..."}
  status           TEXT NOT NULL CHECK (status IN ('draft','scheduled','sending','paused','sent','cancelled')),
  scheduled_at     TEXT,
  started_at       TEXT,
  finished_at      TEXT,
  paused_reason    TEXT NOT NULL DEFAULT '',   -- '' | manual | circuit_breaker | snapshot_failed | ses_paused
  from_name        TEXT,                       -- NULL = settings default
  reply_to         TEXT,                       -- NULL = settings default
  recipients_total INTEGER NOT NULL DEFAULT 0, -- set by the snapshot
  snapshot_job_id  INTEGER,                    -- jobs.id of the broadcast.snapshot job
  snapshot_done_at TEXT,                       -- set when every recipient row has been queued
  version          INTEGER NOT NULL DEFAULT 1,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX broadcasts_status ON broadcasts(status);

-- ---------------------------------------------------------------- messages / tracking

-- AUTOINCREMENT: message IDs are never reused (they are in signed tokens).
CREATE TABLE messages (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  kind            TEXT NOT NULL CHECK (kind IN ('broadcast','sequence','optin','test','alert')),
  broadcast_id    INTEGER REFERENCES broadcasts(id) ON DELETE CASCADE,
  enrollment_id   INTEGER REFERENCES enrollments(id) ON DELETE SET NULL,
  step_id         INTEGER REFERENCES sequence_steps(id) ON DELETE SET NULL,
  email_id        INTEGER REFERENCES emails(id) ON DELETE SET NULL,  -- content source; NULL for system templates
  system_template TEXT NOT NULL DEFAULT '',    -- 'optin_confirm' | 'resubscribe_confirm' | 'alert' | ''
  contact_id      INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  to_email        TEXT,                        -- recipient snapshot; NULLed by GDPR erase
  data            TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data)),  -- extra template data (alert text, confirm URL, test flag...)
  subject         TEXT NOT NULL DEFAULT '',    -- rendered subject, stored at send
  status          TEXT NOT NULL CHECK (status IN ('queued','sending','sent','delivered','bounced','complained','failed','skipped')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,               -- earliest send time (queued_at, or backoff time)
  ses_message_id  TEXT UNIQUE,
  error           TEXT NOT NULL DEFAULT '',
  queued_at       TEXT NOT NULL,
  sent_at         TEXT,
  delivered_at    TEXT,
  updated_at      TEXT NOT NULL,
  UNIQUE (broadcast_id, contact_id),           -- no double broadcast sends
  UNIQUE (enrollment_id, step_id)              -- no double drip steps
);
CREATE INDEX messages_queue ON messages(next_attempt_at, id) WHERE status = 'queued';
CREATE INDEX messages_broadcast ON messages(broadcast_id, status);
CREATE INDEX messages_contact ON messages(contact_id);
CREATE INDEX messages_step ON messages(step_id, status);

CREATE TABLE events (
  id                INTEGER PRIMARY KEY,
  message_id        INTEGER REFERENCES messages(id) ON DELETE CASCADE,
  type              TEXT NOT NULL CHECK (type IN ('delivery','bounce','complaint','reject','unsubscribe','send_error')),
  subtype           TEXT NOT NULL DEFAULT '',  -- bounce: permanent|transient|undetermined; unsubscribe: link|one_click
  provider_event_id TEXT UNIQUE,               -- "<SNS MessageId>:<recipient>" for idempotency (X-6)
  detail            TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(detail)),  -- raw event JSON; emptied after 90 days (X-5) or by erase
  created_at        TEXT NOT NULL
);
CREATE INDEX events_message ON events(message_id);
CREATE INDEX events_type_created ON events(type, created_at);

CREATE TABLE links (
  id         INTEGER PRIMARY KEY,
  url        TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);

CREATE TABLE clicks (
  id         INTEGER PRIMARY KEY,
  link_id    INTEGER NOT NULL REFERENCES links(id) ON DELETE CASCADE,
  message_id INTEGER REFERENCES messages(id) ON DELETE CASCADE,
  contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX clicks_contact ON clicks(contact_id, created_at);
CREATE INDEX clicks_message ON clicks(message_id);
CREATE INDEX clicks_link ON clicks(link_id);

CREATE TABLE opens (
  id         INTEGER PRIMARY KEY,
  message_id INTEGER REFERENCES messages(id) ON DELETE CASCADE,
  contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX opens_message ON opens(message_id);
