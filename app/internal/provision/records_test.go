package provision

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func baseRecordsInput() RecordsInput {
	return RecordsInput{
		Domain:            "Example.com",
		AppSubdomain:      "newsletter",
		MailFromSubdomain: "bounce",
		Region:            "eu-central-1",
		OwnerEmail:        "Owner@Example.com",
		DKIMTokens:        []string{"t1", "t2", "t3"},
	}
}

func TestRecords(t *testing.T) {
	dkim := []Record{
		{Type: "CNAME", Name: "t1._domainkey.example.com", Value: "t1.dkim.amazonses.com", TTL: 300, Purpose: "DKIM signing key (SES Easy DKIM)"},
		{Type: "CNAME", Name: "t2._domainkey.example.com", Value: "t2.dkim.amazonses.com", TTL: 300, Purpose: "DKIM signing key (SES Easy DKIM)"},
		{Type: "CNAME", Name: "t3._domainkey.example.com", Value: "t3.dkim.amazonses.com", TTL: 300, Purpose: "DKIM signing key (SES Easy DKIM)"},
	}
	mailFrom := []Record{
		{Type: "MX", Name: "bounce.example.com", Value: "feedback-smtp.eu-central-1.amazonses.com", TTL: 300, Priority: 10, Purpose: "custom MAIL FROM: receives bounces"},
		{Type: "TXT", Name: "bounce.example.com", Value: "v=spf1 include:amazonses.com ~all", TTL: 300, Purpose: "SPF for the MAIL FROM domain"},
	}
	a := Record{Type: "A", Name: "newsletter.example.com", Value: "203.0.113.7", TTL: 300, Purpose: "Mailroom web app (HTTPS, forms, unsubscribe, SES webhook)"}
	aaaa := Record{Type: "AAAA", Name: "newsletter.example.com", Value: "2001:db8::1", TTL: 300, Purpose: "Mailroom web app over IPv6"}
	dmarc := Record{Type: "TXT", Name: "_dmarc.example.com", Value: "v=DMARC1; p=none; rua=mailto:owner@example.com", TTL: 300, Purpose: "DMARC policy (monitoring only)"}
	noRUA := Record{Type: "TXT", Name: "_dmarc.example.com", Value: "v=DMARC1; p=none", TTL: 300,
		Purpose: "DMARC policy (monitoring only; no aggregate reports, as OWNER_EMAIL is not on the domain)"}

	cat := func(parts ...[]Record) []Record {
		var out []Record
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	tests := []struct {
		name string
		mod  func(*RecordsInput)
		want []Record
	}{
		{"minimal", func(*RecordsInput) {}, cat(dkim, mailFrom)},
		{"ipv4 ipv6 dmarc", func(in *RecordsInput) {
			in.IPv4, in.IPv6, in.DMARC = "203.0.113.7", "2001:DB8:0::1", true
		}, cat([]Record{a, aaaa}, dkim, mailFrom, []Record{dmarc})},
		{"dmarc owner on a subdomain", func(in *RecordsInput) { in.DMARC, in.OwnerEmail = true, "Me@Mail.Example.com" },
			cat(dkim, mailFrom, []Record{{Type: "TXT", Name: "_dmarc.example.com", Value: "v=DMARC1; p=none; rua=mailto:me@mail.example.com", TTL: 300, Purpose: "DMARC policy (monitoring only)"}})},
		// A report address on another domain would need an authorization
		// record there (RFC 7489 §7.1): no rua.
		{"dmarc owner off the domain", func(in *RecordsInput) { in.DMARC, in.OwnerEmail = true, "owner@gmail.com" }, cat(dkim, mailFrom, []Record{noRUA})},
		{"dmarc look-alike domain", func(in *RecordsInput) { in.DMARC, in.OwnerEmail = true, "owner@notexample.com" }, cat(dkim, mailFrom, []Record{noRUA})},
		{"dmarc without owner", func(in *RecordsInput) { in.DMARC, in.OwnerEmail = true, "" }, cat(dkim, mailFrom, []Record{noRUA})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseRecordsInput()
			tt.mod(&in)
			if err := in.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := Records(in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Records =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestDKIMDomain(t *testing.T) {
	tests := []struct{ zone, region, want string }{
		{"", "eu-central-1", "dkim.amazonses.com"},
		{"", "eu-south-1", "dkim.eu-south-1.amazonses.com"},
		{"dkim.custom.example.", "eu-central-1", "dkim.custom.example"},
	}
	for _, tt := range tests {
		if got := dkimDomain(tt.zone, tt.region); got != tt.want {
			t.Errorf("dkimDomain(%q, %q) = %q, want %q", tt.zone, tt.region, got, tt.want)
		}
	}
}

func TestRecordsInputValidate(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*RecordsInput)
		want string
	}{
		{"ipv6 as ipv4", func(in *RecordsInput) { in.IPv4 = "2001:db8::1" }, "--ipv4"},
		{"ipv4 as ipv6", func(in *RecordsInput) { in.IPv6 = "203.0.113.7" }, "--ipv6"},
		{"garbage ip", func(in *RecordsInput) { in.IPv4 = "server" }, "--ipv4"},
		{"no region", func(in *RecordsInput) { in.Region = "" }, "AWS_REGION"},
		{"bad domain", func(in *RecordsInput) { in.Domain = "bad_domain" }, "DOMAIN"},
		{"bad app subdomain", func(in *RecordsInput) { in.IPv4, in.AppSubdomain = "203.0.113.7", "a..b" }, "APP_SUBDOMAIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseRecordsInput()
			tt.mod(&in)
			err := in.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWriteRecordsTable(t *testing.T) {
	in := baseRecordsInput()
	in.IPv4 = "203.0.113.7"
	var buf bytes.Buffer
	if err := WriteRecordsTable(&buf, Records(in)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("table has %d lines:\n%s", len(lines), buf.String())
	}
	if f := strings.Fields(lines[0]); !reflect.DeepEqual(f, []string{"TYPE", "NAME", "VALUE", "PRIORITY", "TTL", "PURPOSE"}) {
		t.Errorf("header = %v", f)
	}
	// Columns are aligned: every VALUE starts at the same offset.
	col := strings.Index(lines[0], "VALUE")
	for _, l := range lines[1:] {
		if l[col-1] != ' ' || l[col] == ' ' {
			t.Errorf("misaligned line %q", l)
		}
	}
	if !strings.Contains(buf.String(), "MX     bounce.example.com") || !strings.Contains(lines[5], " 10 ") {
		t.Errorf("MX row missing its priority:\n%s", buf.String())
	}
	if !strings.Contains(lines[6], "v=spf1 include:amazonses.com ~all") {
		t.Errorf("SPF row = %q", lines[6])
	}
}
