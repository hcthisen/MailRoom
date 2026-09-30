-- Mailroom schema v3: stored SES events lose the original message headers.
-- SES copies the headers into every event (mail.headers and
-- mail.commonHeaders), and they include List-Unsubscribe with the
-- recipient's working one-click unsubscribe URL. Events are readable with a
-- read-scope token, so the headers are no longer stored (events.withoutHeaders)
-- and are removed here from the rows stored before.
UPDATE events
   SET detail = json_remove(detail, '$.mail.headers', '$.mail.commonHeaders', '$.mail.headersTruncated')
 WHERE json_type(detail, '$.mail') = 'object'
   AND (json_type(detail, '$.mail.headers') IS NOT NULL
        OR json_type(detail, '$.mail.commonHeaders') IS NOT NULL);
