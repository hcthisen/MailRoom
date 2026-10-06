-- Mailroom schema v2: IDs of segments, emails, broadcasts and sequence steps
-- are never reused (AUTOINCREMENT), like those of contacts and messages.
-- They are referenced by ID from stored JSON and rules that outlive the row:
-- a broadcast's audience (segment_id), segment rules (clicked.broadcast_id),
-- message rows (email_id, step_id). Without AUTOINCREMENT SQLite hands the
-- ID of a deleted row with the highest ID to the next new row, so such a
-- reference would silently point at a different segment, broadcast or step.
--
-- The standard rebuild (create, copy, drop, rename). The migration runner
-- switches foreign key enforcement off around it, so the drops don't
-- cascade into sequence_steps, broadcasts or messages, and checks
-- PRAGMA foreign_key_check before it commits. The copies keep every ID, and
-- AUTOINCREMENT starts after the highest one.

CREATE TABLE new_segments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  filter     TEXT NOT NULL CHECK (json_valid(filter)),
  version    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO new_segments (id, name, filter, version, created_at, updated_at)
  SELECT id, name, filter, version, created_at, updated_at FROM segments;
DROP TABLE segments;
ALTER TABLE new_segments RENAME TO segments;

CREATE TABLE new_emails (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL DEFAULT '',
  subject    TEXT NOT NULL,
  preheader  TEXT NOT NULL DEFAULT '',
  format     TEXT NOT NULL CHECK (format IN ('markdown','html')),
  body       TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO new_emails (id, name, subject, preheader, format, body, version, created_at, updated_at)
  SELECT id, name, subject, preheader, format, body, version, created_at, updated_at FROM emails;
DROP TABLE emails;
ALTER TABLE new_emails RENAME TO emails;

CREATE TABLE new_sequence_steps (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  sequence_id   INTEGER NOT NULL REFERENCES sequences(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,              -- 1-based; reorder via a two-step update
  delay_seconds INTEGER NOT NULL CHECK (delay_seconds >= 0),
  email_id      INTEGER NOT NULL REFERENCES emails(id),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  UNIQUE (sequence_id, position)
);
INSERT INTO new_sequence_steps (id, sequence_id, position, delay_seconds, email_id, created_at, updated_at)
  SELECT id, sequence_id, position, delay_seconds, email_id, created_at, updated_at FROM sequence_steps;
DROP TABLE sequence_steps;
ALTER TABLE new_sequence_steps RENAME TO sequence_steps;

CREATE TABLE new_broadcasts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
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
INSERT INTO new_broadcasts (id, name, email_id, audience, status, scheduled_at, started_at, finished_at, paused_reason,
                            from_name, reply_to, recipients_total, snapshot_job_id, snapshot_done_at, version,
                            created_at, updated_at)
  SELECT id, name, email_id, audience, status, scheduled_at, started_at, finished_at, paused_reason,
         from_name, reply_to, recipients_total, snapshot_job_id, snapshot_done_at, version, created_at, updated_at
  FROM broadcasts;
DROP TABLE broadcasts;
ALTER TABLE new_broadcasts RENAME TO broadcasts;
CREATE INDEX broadcasts_status ON broadcasts(status);
