-- Mailroom schema v5: receiving email (L-6, N-1..N-6).
--
-- inbound_messages holds the mail SES received for DOMAIN. The SNS webhook
-- inserts a 'pending' row from the notification; the inbound-fetcher
-- worker then fetches the stored message from S3, parses it, and fills in
-- the bodies (status 'received'), or gives up ('failed'). Attachments are
-- kept in the database too (inbound_attachments), so backups, a contact's
-- erase and deleting a message need nothing outside SQLite.
CREATE TABLE inbound_messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  s3_key            TEXT NOT NULL UNIQUE,          -- object key in INBOUND_BUCKET
  ses_message_id    TEXT NOT NULL DEFAULT '',
  sns_message_id    TEXT NOT NULL DEFAULT '',
  from_email        TEXT NOT NULL DEFAULT '',      -- normalised when it parses, else as given
  from_name         TEXT NOT NULL DEFAULT '',
  to_addrs          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(to_addrs)),
  cc_addrs          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(cc_addrs)),
  reply_to          TEXT NOT NULL DEFAULT '',
  subject           TEXT NOT NULL DEFAULT '',
  message_id_header TEXT NOT NULL DEFAULT '',      -- Message-ID of the received mail
  in_reply_to       TEXT NOT NULL DEFAULT '',
  references_header TEXT NOT NULL DEFAULT '',
  sent_date         TEXT,                          -- the Date header, when it parses
  received_at       TEXT NOT NULL,                 -- when SES received it
  text_body         TEXT NOT NULL DEFAULT '',
  html_body         TEXT NOT NULL DEFAULT '',
  snippet           TEXT NOT NULL DEFAULT '',
  size_bytes        INTEGER NOT NULL DEFAULT 0,
  spam_verdict      TEXT NOT NULL DEFAULT '',      -- PASS | FAIL | GRAY | PROCESSING_FAILED | DISABLED
  virus_verdict     TEXT NOT NULL DEFAULT '',
  spf_verdict       TEXT NOT NULL DEFAULT '',
  dkim_verdict      TEXT NOT NULL DEFAULT '',
  dmarc_verdict     TEXT NOT NULL DEFAULT '',
  contact_id        INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  status            TEXT NOT NULL CHECK (status IN ('pending','received','failed')),
  attempts          INTEGER NOT NULL DEFAULT 0,
  next_attempt_at   TEXT NOT NULL,
  fetch_error       TEXT NOT NULL DEFAULT '',
  read_at           TEXT,
  archived_at       TEXT,
  version           INTEGER NOT NULL DEFAULT 1,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);
CREATE INDEX inbound_pending ON inbound_messages(next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX inbound_received ON inbound_messages(received_at, id);
CREATE INDEX inbound_contact ON inbound_messages(contact_id);
CREATE INDEX inbound_from ON inbound_messages(from_email);

CREATE TABLE inbound_attachments (
  id           INTEGER PRIMARY KEY,
  inbound_id   INTEGER NOT NULL REFERENCES inbound_messages(id) ON DELETE CASCADE,
  position     INTEGER NOT NULL,                   -- 1-based, in message order
  filename     TEXT NOT NULL,                      -- sanitised
  content_type TEXT NOT NULL,
  content_id   TEXT NOT NULL DEFAULT '',           -- for inline images (cid:)
  inline       INTEGER NOT NULL DEFAULT 0,
  size_bytes   INTEGER NOT NULL,
  data         BLOB NOT NULL,
  UNIQUE (inbound_id, position)
);

-- Replies sent from the inbox are messages of the new kind 'reply' (N-5).
-- SQLite can't change a CHECK constraint, so messages is rebuilt the
-- standard way (the runner turns foreign keys off around it). Every ID is
-- kept, and the AUTOINCREMENT counter is carried over: signed tokens carry
-- message IDs, so an ID must never be handed out twice.
CREATE TABLE new_messages (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  kind            TEXT NOT NULL CHECK (kind IN ('broadcast','sequence','optin','test','alert','reply')),
  broadcast_id    INTEGER REFERENCES broadcasts(id) ON DELETE CASCADE,
  enrollment_id   INTEGER REFERENCES enrollments(id) ON DELETE SET NULL,
  step_id         INTEGER REFERENCES sequence_steps(id) ON DELETE SET NULL,
  email_id        INTEGER REFERENCES emails(id) ON DELETE SET NULL,
  system_template TEXT NOT NULL DEFAULT '',
  contact_id      INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
  to_email        TEXT,
  data            TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data)),
  subject         TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL CHECK (status IN ('queued','sending','sent','delivered','bounced','complained','failed','skipped')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT NOT NULL,
  ses_message_id  TEXT UNIQUE,
  error           TEXT NOT NULL DEFAULT '',
  queued_at       TEXT NOT NULL,
  sent_at         TEXT,
  delivered_at    TEXT,
  updated_at      TEXT NOT NULL,
  inbound_id      INTEGER REFERENCES inbound_messages(id) ON DELETE SET NULL,  -- the received mail a reply answers
  UNIQUE (broadcast_id, contact_id),
  UNIQUE (enrollment_id, step_id)
);
INSERT INTO new_messages (id, kind, broadcast_id, enrollment_id, step_id, email_id, system_template, contact_id, to_email,
    data, subject, status, attempts, next_attempt_at, ses_message_id, error, queued_at, sent_at, delivered_at, updated_at)
  SELECT id, kind, broadcast_id, enrollment_id, step_id, email_id, system_template, contact_id, to_email,
    data, subject, status, attempts, next_attempt_at, ses_message_id, error, queued_at, sent_at, delivered_at, updated_at
  FROM messages;
INSERT INTO sqlite_sequence (name, seq)
  SELECT 'new_messages', seq FROM sqlite_sequence WHERE name = 'messages'
   AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'new_messages');
UPDATE sqlite_sequence
   SET seq = (SELECT seq FROM sqlite_sequence WHERE name = 'messages')
 WHERE name = 'new_messages'
   AND seq < (SELECT seq FROM sqlite_sequence WHERE name = 'messages');
DROP TABLE messages;
ALTER TABLE new_messages RENAME TO messages;
CREATE INDEX messages_queue ON messages(next_attempt_at, id) WHERE status = 'queued';
CREATE INDEX messages_broadcast ON messages(broadcast_id, status);
CREATE INDEX messages_contact ON messages(contact_id);
CREATE INDEX messages_step ON messages(step_id, status);
CREATE INDEX messages_inbound ON messages(inbound_id) WHERE inbound_id IS NOT NULL;
