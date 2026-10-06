package config

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ACMEPort is the port of the plain-HTTP listener in autocert mode: it
// answers HTTP-01 challenges and /healthz and redirects everything else
// to HTTPS.
const ACMEPort = "80"

// HealthURL returns the URL at which a Mailroom server configured by
// getenv answers /healthz from inside its own host or container: the
// HTTP_ADDR listener with HTTP_ONLY=1 (httpOnly forces that mode, like
// "serve --http-only"), otherwise the autocert HTTP listener on port 80.
// A wildcard or empty listen host becomes the loopback address. It reads
// only HTTP_ONLY and HTTP_ADDR, so it works without the rest of the
// configuration (the container health check runs it).
func HealthURL(getenv func(string) string, httpOnly bool) (string, error) {
	if !httpOnly {
		v, err := parseBool(getenv("HTTP_ONLY"))
		if err != nil {
			return "", fmt.Errorf("HTTP_ONLY: %w", err)
		}
		httpOnly = v
	}
	if !httpOnly {
		return "http://127.0.0.1:" + ACMEPort + "/healthz", nil
	}
	addr := strings.TrimSpace(getenv("HTTP_ADDR"))
	if addr == "" {
		addr = DefaultHTTPAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", fmt.Errorf("HTTP_ADDR %q is not host:port", addr)
	}
	if ip, err := netip.ParseAddr(host); host == "" || (err == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
