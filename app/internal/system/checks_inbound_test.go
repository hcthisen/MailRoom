package system

import (
	"net"
	"strings"
	"testing"
	"time"

	"mailroom/internal/db"
	"mailroom/internal/testutil"
)

func TestDoctorInbound(t *testing.T) {
	check := func(f *fixture) Check {
		t.Helper()
		for _, c := range f.service().Doctor(f.ctx).Checks {
			if c.Name == CheckInbound {
				return c
			}
		}
		t.Fatal("no inbound check")
		return Check{}
	}

	f := healthyFixture(t)
	if c := check(f); c.Status != StatusOK {
		t.Fatalf("healthy = %+v", c)
	}

	f.resolver.mx["example.com"] = []*net.MX{{Host: "aspmx.l.google.com.", Pref: 1}}
	if c := check(f); c.Status != StatusFail || !strings.Contains(c.Message, "aspmx.l.google.com") || !strings.Contains(c.FixHint, "--no-receive") {
		t.Errorf("foreign MX = %+v", c)
	}
	delete(f.resolver.mx, "example.com")
	if c := check(f); c.Status != StatusFail || !strings.Contains(c.FixHint, "MX 10 inbound-smtp.eu-west-1.amazonaws.com") {
		t.Errorf("no MX = %+v", c)
	}

	f = healthyFixture(t)
	old := db.FormatTime(f.clock.Now().Add(-time.Hour))
	testutil.Exec(t, f.d, `INSERT INTO inbound_messages (s3_key, status, received_at, next_attempt_at, created_at, updated_at)
		VALUES ('inbound/a', 'pending', ?, ?, ?, ?), ('inbound/b', 'failed', ?, ?, ?, ?)`, old, old, old, old, old, old, old, old)
	if c := check(f); c.Status != StatusWarn || !strings.Contains(c.Message, "waited over 15 minutes") || !strings.Contains(c.Message, "could not be fetched") {
		t.Errorf("stuck = %+v", c)
	}

	f = healthyFixture(t)
	f.cfg.InboundBucket, f.cfg.InboundDomain = "", ""
	if c := check(f); c.Status != StatusSkipped {
		t.Errorf("off = %+v", c)
	}
}
