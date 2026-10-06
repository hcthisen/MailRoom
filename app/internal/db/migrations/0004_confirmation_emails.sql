-- Mailroom schema v4: a log of the confirmation emails the public forms
-- sent, for their caps (forms.sendAllowed): per address, keyed by the
-- address hash so the count survives the deletion of an expired pending
-- contact (whose messages lose their address), and per hour, of all forms
-- and per form. Rows are deleted after 31 days (system retention).
CREATE TABLE confirmation_emails (
  id         INTEGER PRIMARY KEY,
  email_hash TEXT NOT NULL,             -- core.EmailHash of the address
  form_id    INTEGER,                   -- the form (no foreign key: forms may be deleted)
  kind       TEXT NOT NULL CHECK (kind IN ('optin','resubscribe')),
  queued_at  TEXT NOT NULL
);
CREATE INDEX confirmation_emails_address ON confirmation_emails(email_hash, queued_at);
CREATE INDEX confirmation_emails_queued ON confirmation_emails(queued_at);
