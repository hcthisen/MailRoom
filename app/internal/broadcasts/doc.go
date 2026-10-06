// Package broadcasts implements Mailroom's broadcasts (PRD §6.5, B-1 to
// B-10): one email sent to everyone active or to a segment, now or at a
// scheduled time.
//
// A broadcast owns its email: it is created, changed, copied and deleted
// together with the broadcast through the content service.
//
// State machine (B-2):
//
//	draft ──schedule──▶ scheduled ──unschedule──▶ draft
//	draft | scheduled ──send (or the starter at scheduled_at)──▶ sending
//	sending ──pause (manual, circuit breaker)──▶ paused ──resume──▶ sending
//	draft | scheduled | paused ──cancel──▶ cancelled
//	sending ──(sending package, once every message is out)──▶ sent
//
// Only the sending package sets "sent" (sending.CompleteBroadcasts); no
// API call does.
//
// Snapshot (B-3): starting a broadcast submits the resumable job
// "broadcast.snapshot" in the same transaction. The job pages through the
// active, unsuppressed contacts of the audience by ID and queues one
// broadcast message per contact through outbox.Enqueue, 500 per
// transaction. The transaction that queues the last batch also sets
// recipients_total and snapshot_done_at, so sending can complete the
// broadcast. Enqueue is idempotent per broadcast and contact, so a resumed
// or repeated job never queues a contact twice.
//
// The broadcast-starter worker (every 10 s) starts due scheduled
// broadcasts, re-submits snapshots whose job failed, and runs the circuit
// breaker (B-6): a sending broadcast is paused, and the owner alerted, when
// its hard-bounce or complaint rate exceeds the configured threshold once
// at least circuit_breaker.min_messages of it have been sent.
//
// Web archive (B-10): ArchiveHandler serves /archive/{id}, the email of a
// broadcast to everyone that has gone out, rendered without contact data
// (merge fields show their fallbacks) and without tracking.
//
// The email of a broadcast can only change while it is a draft or
// scheduled: content.UpdateEmail refuses other edits, also through
// /emails. Test sends (B-7) carry the broadcast's sender overrides.
package broadcasts
