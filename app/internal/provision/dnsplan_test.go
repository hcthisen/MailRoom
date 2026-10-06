package provision

import (
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

func boolp(b bool) *bool { return &b }

func TestPlanDNS(t *testing.T) {
	in := baseRecordsInput()
	in.IPv4, in.IPv6, in.DMARC = "203.0.113.7", "2001:db8::1", true
	desired := Records(in)
	// desired: A, AAAA, 3x DKIM CNAME, MX, SPF TXT, DMARC TXT

	type counts struct{ create, unchanged, conflicts int }
	tests := []struct {
		name      string
		existing  []DNSRecord
		want      counts
		conflicts map[string]string // "TYPE name" -> reason substring
	}{
		{
			name:     "empty zone",
			existing: nil,
			want:     counts{8, 0, 0},
		},
		{
			name: "everything present, formatting differences ignored",
			existing: []DNSRecord{
				{Type: "A", Name: "Newsletter.Example.com.", Content: "203.0.113.7"},
				{Type: "AAAA", Name: "newsletter.example.com", Content: "2001:0db8:0:0::1"},
				{Type: "CNAME", Name: "t1._domainkey.example.com", Content: "T1.dkim.amazonses.com."},
				{Type: "CNAME", Name: "t2._domainkey.example.com", Content: "t2.dkim.amazonses.com"},
				{Type: "CNAME", Name: "t3._domainkey.example.com", Content: "t3.dkim.amazonses.com"},
				{Type: "MX", Name: "bounce.example.com", Content: "feedback-smtp.eu-central-1.amazonses.com", Priority: intp(10)},
				{Type: "TXT", Name: "bounce.example.com", Content: `"v=spf1 include:amazonses.com" " ~all"`},
				{Type: "TXT", Name: "_dmarc.example.com", Content: "v=DMARC1; p=none; rua=mailto:owner@example.com"},
			},
			want: counts{0, 8, 0},
		},
		{
			name: "different content is a conflict",
			existing: []DNSRecord{
				{Type: "A", Name: "newsletter.example.com", Content: "198.51.100.1"},
				{Type: "CNAME", Name: "t1._domainkey.example.com", Content: "other.example.net"},
				{Type: "MX", Name: "bounce.example.com", Content: "mx.other.example", Priority: intp(10)},
				{Type: "TXT", Name: "bounce.example.com", Content: "v=spf1 include:_spf.google.com ~all"},
			},
			want: counts{4, 0, 4},
			conflicts: map[string]string{
				"A newsletter.example.com":        "has different content",
				"CNAME t1._domainkey.example.com": "has different content",
				"MX bounce.example.com":           "has different content",
				"TXT bounce.example.com":          "SPF",
			},
		},
		{
			name: "MX with another priority conflicts",
			existing: []DNSRecord{
				{Type: "MX", Name: "bounce.example.com", Content: "feedback-smtp.eu-central-1.amazonses.com", Priority: intp(20)},
			},
			want:      counts{7, 0, 1},
			conflicts: map[string]string{"MX bounce.example.com": "has different content"},
		},
		{
			name: "CNAME clashes both ways",
			existing: []DNSRecord{
				{Type: "CNAME", Name: "newsletter.example.com", Content: "old.example.net"},
				{Type: "TXT", Name: "t2._domainkey.example.com", Content: "v=DKIM1; p=abc"},
			},
			want: counts{5, 0, 3},
			conflicts: map[string]string{
				"A newsletter.example.com":        "has a CNAME",
				"AAAA newsletter.example.com":     "has a CNAME",
				"CNAME t2._domainkey.example.com": "cannot coexist with a CNAME",
			},
		},
		{
			name: "matching but proxied A/AAAA/CNAME is a conflict",
			existing: []DNSRecord{
				{Type: "A", Name: "newsletter.example.com", Content: "203.0.113.7", Proxied: boolp(true)},
				{Type: "AAAA", Name: "newsletter.example.com", Content: "2001:db8::1", Proxied: boolp(false)},
				{Type: "CNAME", Name: "t1._domainkey.example.com", Content: "t1.dkim.amazonses.com", Proxied: boolp(true)},
				{Type: "CNAME", Name: "t2._domainkey.example.com", Content: "t2.dkim.amazonses.com"},
			},
			want: counts{4, 2, 2},
			conflicts: map[string]string{
				"A newsletter.example.com":        "proxied through Cloudflare",
				"CNAME t1._domainkey.example.com": "proxied through Cloudflare",
			},
		},
		{
			name: "other TXT at the MAIL FROM name does not block SPF",
			existing: []DNSRecord{
				{Type: "TXT", Name: "bounce.example.com", Content: "google-site-verification=abc"},
			},
			want: counts{8, 0, 0},
		},
		{
			name: "existing DMARC is kept, never conflicting",
			existing: []DNSRecord{
				{Type: "TXT", Name: "_dmarc.example.com", Content: "v=DMARC1; p=reject"},
			},
			want: counts{7, 1, 0},
		},
		{
			name: "records elsewhere are ignored",
			existing: []DNSRecord{
				{Type: "A", Name: "example.com", Content: "198.51.100.1"},
				{Type: "MX", Name: "example.com", Content: "mx.example.com", Priority: intp(10)},
				{Type: "TXT", Name: "example.com", Content: "v=spf1 -all"},
			},
			want: counts{8, 0, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PlanDNS(tt.existing, desired)
			got := counts{len(p.Create), len(p.Unchanged), len(p.Conflicts)}
			if got != tt.want {
				t.Fatalf("counts = %+v, want %+v\nplan %+v", got, tt.want, p)
			}
			for _, c := range p.Conflicts {
				key := c.Record.Type + " " + c.Record.Name
				want, ok := tt.conflicts[key]
				if !ok {
					t.Errorf("unexpected conflict %s: %s", key, c.Reason)
					continue
				}
				if !strings.Contains(c.Reason, want) || len(c.Existing) == 0 {
					t.Errorf("conflict %s reason %q (existing %v), want %q", key, c.Reason, c.Existing, want)
				}
			}
			if len(p.Conflicts) != len(tt.conflicts) {
				t.Errorf("conflicts = %+v, want %v", p.Conflicts, tt.conflicts)
			}
			// Every desired record lands in exactly one bucket.
			if n := len(p.Create) + len(p.Unchanged) + len(p.Conflicts); n != len(desired) {
				t.Errorf("plan covers %d of %d records", n, len(desired))
			}
		})
	}
}

func TestTXTContent(t *testing.T) {
	tests := map[string]string{
		`v=spf1 -all`:          "v=spf1 -all",
		`"v=spf1 -all"`:        "v=spf1 -all",
		`"v=spf1 " "-all"`:     "v=spf1 -all",
		`"say \"hi\""`:         `say "hi"`,
		`  "padded"  `:         "padded",
		`"a"   "b"   "c"`:      "abc",
		`not "quoted" at all`:  `not "quoted" at all`,
		`"unterminated string`: "unterminated string",
	}
	for in, want := range tests {
		if got := txtContent(in); got != want {
			t.Errorf("txtContent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToCloudflare(t *testing.T) {
	in := baseRecordsInput()
	in.IPv4 = "203.0.113.7"
	for _, r := range Records(in) {
		c := r.ToCloudflare()
		switch r.Type {
		case "A", "CNAME":
			if c.Proxied == nil || *c.Proxied {
				t.Errorf("%s %s must be DNS-only", r.Type, r.Name)
			}
		case "MX":
			if c.Priority == nil || *c.Priority != 10 || c.Proxied != nil {
				t.Errorf("MX = %+v", c)
			}
		case "TXT":
			if c.Proxied != nil || c.Priority != nil {
				t.Errorf("TXT = %+v", c)
			}
		}
		if c.Content != r.Value || c.Name != r.Name || c.TTL != r.TTL || len(c.Comment) > maxCommentLength || c.Comment == "" {
			t.Errorf("converted %+v -> %+v", r, c)
		}
	}
}
