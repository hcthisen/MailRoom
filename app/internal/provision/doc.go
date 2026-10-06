// Package provision implements Mailroom's provisioning CLI (PRD §6.13
// L-1, L-2, L-3 and the IAM rules in §11):
//
//   - "provision ses" sets up the SES identity (Easy DKIM, custom MAIL FROM),
//     the configuration set with its SNS event destination, the SNS topic,
//     the optional S3 backup bucket and the least-privilege sending IAM user
//     mailroom-sender, and writes the sending user's keys to the server env
//     file. Every step describes first, creates what is missing and
//     converges what differs, so running it again is safe.
//   - "provision ses --request-production" submits the SES production
//     access request.
//   - "provision dns --cloudflare" creates the missing DNS records through
//     the Cloudflare API. It never updates or deletes a record.
//   - "dns records" prints the same records for manual DNS.
//
// With --json every command prints exactly one JSON document on stdout,
// also on failure: the command's result with an "error" field, or
// {"error": "..."} when it failed before producing one.
//
// The AWS SDK clients are reached through narrow interfaces (STSAPI,
// SESAPI, SNSAPI, IAMAPI, S3API) so tests use fakes. The package has no
// HTTP API; cmd/mailroom registers ProvisionCommand and DNSCommand.
package provision
