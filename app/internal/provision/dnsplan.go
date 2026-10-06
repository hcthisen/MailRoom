package provision

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// Plan is the difference between the records Mailroom needs and a zone.
type Plan struct {
	// Create are the records that do not exist yet.
	Create []Record `json:"create"`
	// Unchanged are needed records already present (or, for DMARC, an
	// existing policy that is kept as it is).
	Unchanged []Match `json:"unchanged"`
	// Conflicts are needed records that clash with existing ones. They are
	// never created or changed automatically.
	Conflicts []Conflict `json:"conflicts"`
}

// Match pairs a needed record with the existing record that satisfies it.
type Match struct {
	Record   Record    `json:"record"`
	Existing DNSRecord `json:"existing"`
	Note     string    `json:"note,omitempty"`
}

// Conflict is a needed record blocked by existing records.
type Conflict struct {
	Record   Record      `json:"record"`
	Existing []DNSRecord `json:"existing"`
	Reason   string      `json:"reason"`
}

// PlanDNS compares the desired records with the existing ones:
//   - a record whose name, type and content exist is unchanged, unless it
//     is an A/AAAA/CNAME record proxied through Cloudflare (a conflict);
//   - an A/AAAA/CNAME/MX record whose name and type exist with other
//     content is a conflict, as is a CNAME next to any other record at the
//     same name (in either direction) and an SPF TXT at a name that already
//     has a different SPF record;
//   - a DMARC record is only proposed when the name has no TXT record;
//   - everything else is created.
func PlanDNS(existing []DNSRecord, desired []Record) Plan {
	p := Plan{Create: []Record{}, Unchanged: []Match{}, Conflicts: []Conflict{}}
	for _, d := range desired {
		name := dnsName(d.Name)
		var atName []DNSRecord
		for _, e := range existing {
			if dnsName(e.Name) == name {
				atName = append(atName, e)
			}
		}
		if c, ok := cnameClash(d, atName); ok {
			p.Conflicts = append(p.Conflicts, c)
			continue
		}
		var same []DNSRecord
		for _, e := range atName {
			if strings.EqualFold(e.Type, d.Type) {
				same = append(same, e)
			}
		}
		if e, ok := firstMatching(d, same); ok {
			if proxied(d, e) {
				p.Conflicts = append(p.Conflicts, Conflict{Record: d, Existing: []DNSRecord{e}, Reason: "the record exists but is proxied through Cloudflare; switch it to DNS only"})
				continue
			}
			p.Unchanged = append(p.Unchanged, Match{Record: d, Existing: e})
			continue
		}
		switch {
		case d.Type == "TXT" && strings.HasPrefix(name, "_dmarc."):
			if len(same) > 0 {
				p.Unchanged = append(p.Unchanged, Match{Record: d, Existing: same[0], Note: "an existing DMARC record is kept as it is"})
				continue
			}
		case d.Type == "TXT" && isSPF(d.Value):
			if spf := filter(same, func(e DNSRecord) bool { return isSPF(txtContent(e.Content)) }); len(spf) > 0 {
				p.Conflicts = append(p.Conflicts, Conflict{Record: d, Existing: spf, Reason: "a different SPF record exists at this name; merge include:amazonses.com into it by hand"})
				continue
			}
		case d.Type == "TXT":
		default:
			if len(same) > 0 {
				p.Conflicts = append(p.Conflicts, Conflict{Record: d, Existing: same, Reason: fmt.Sprintf("an existing %s record at this name has different content", d.Type)})
				continue
			}
		}
		p.Create = append(p.Create, d)
	}
	return p
}

// proxied reports whether an existing A/AAAA/CNAME record goes through
// Cloudflare's proxy: a proxied DKIM CNAME is flattened, so the DKIM key
// cannot be looked up, and a proxied app record breaks Mailroom's own TLS.
func proxied(d Record, e DNSRecord) bool {
	switch d.Type {
	case "A", "AAAA", "CNAME":
		return e.Proxied != nil && *e.Proxied
	}
	return false
}

func cnameClash(d Record, atName []DNSRecord) (Conflict, bool) {
	var clash []DNSRecord
	for _, e := range atName {
		isCNAME := strings.EqualFold(e.Type, "CNAME")
		if (d.Type == "CNAME") != isCNAME {
			clash = append(clash, e)
		}
	}
	if len(clash) == 0 {
		return Conflict{}, false
	}
	reason := "this name has a CNAME, which cannot coexist with other records"
	if d.Type == "CNAME" {
		reason = "this name has other records, which cannot coexist with a CNAME"
	}
	return Conflict{Record: d, Existing: clash, Reason: reason}, true
}

func firstMatching(d Record, same []DNSRecord) (DNSRecord, bool) {
	for _, e := range same {
		if contentEqual(d, e) {
			return e, true
		}
	}
	return DNSRecord{}, false
}

func contentEqual(d Record, e DNSRecord) bool {
	switch d.Type {
	case "A", "AAAA":
		a, err1 := netip.ParseAddr(d.Value)
		b, err2 := netip.ParseAddr(strings.TrimSpace(e.Content))
		return err1 == nil && err2 == nil && a == b
	case "CNAME":
		return dnsName(d.Value) == dnsName(e.Content)
	case "MX":
		return dnsName(d.Value) == dnsName(e.Content) && (e.Priority == nil || *e.Priority == d.Priority)
	case "TXT":
		return txtContent(e.Content) == d.Value
	default:
		return strings.TrimSpace(e.Content) == d.Value
	}
}

// dnsName lowercases a name and drops a trailing dot.
func dnsName(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), ".")) }

func isSPF(v string) bool { return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "v=spf1") }

// txtContent removes TXT quoting: `"a" "b"` becomes `ab`.
func txtContent(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	var b strings.Builder
	in, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case in && r == '\\':
			esc = true
		case r == '"':
			in = !in
		case in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func filter(recs []DNSRecord, keep func(DNSRecord) bool) []DNSRecord {
	var out []DNSRecord
	for _, r := range recs {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// maxCommentLength is Cloudflare's record comment limit on every plan.
const maxCommentLength = 100

// ToCloudflare converts a Record for creation: A, AAAA and CNAME records
// are DNS-only (never proxied), and the comment names its purpose.
func (r Record) ToCloudflare() DNSRecord {
	out := DNSRecord{Type: r.Type, Name: r.Name, Content: r.Value, TTL: r.TTL}
	switch r.Type {
	case "A", "AAAA", "CNAME":
		f := false
		out.Proxied = &f
	case "MX":
		p := r.Priority
		out.Priority = &p
	}
	comment := "mailroom: " + r.Purpose
	if len(comment) > maxCommentLength {
		comment = comment[:maxCommentLength]
	}
	out.Comment = comment
	return out
}

// ApplyDNS creates the plan's missing records and nothing else: it never
// updates or deletes a record, and conflicts are left for a human. It
// returns the records created before any error.
func ApplyDNS(ctx context.Context, cf *Cloudflare, zoneID string, plan Plan) ([]DNSRecord, error) {
	created := []DNSRecord{}
	for _, r := range plan.Create {
		rec, err := cf.CreateRecord(ctx, zoneID, r.ToCloudflare())
		if err != nil {
			return created, err
		}
		created = append(created, rec)
	}
	return created, nil
}
