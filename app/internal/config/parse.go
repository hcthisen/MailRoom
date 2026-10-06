package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// parsePrefixes parses a comma-separated list of CIDR prefixes or single
// IP addresses (a single address is a /32 or /128 prefix).
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", f)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR prefix", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// parseBool accepts the usual spellings of a boolean flag; empty is false.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, fmt.Errorf("%q is not a boolean (use 1 or 0)", s)
}

// baseURL returns the override (normalised) or the derived https URL.
func baseURL(override, sub, domain string, httpOnly bool) (string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		if domain == "" {
			return "", nil
		}
		return "https://" + sub + "." + domain, nil
	}
	u, err := url.Parse(override)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%q must be an absolute http(s) URL", override)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("%q must be a scheme and host only, without path, query or credentials", override)
	}
	if u.Scheme == "http" && !httpOnly {
		return "", fmt.Errorf("%q uses http, which needs HTTP_ONLY=1 (autocert serves https only)", override)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// validLabel reports whether s is one DNS label (letters, digits, hyphens).
func validLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validHostname reports whether s is a dot-separated list of DNS labels.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if !validLabel(l) {
			return false
		}
	}
	return true
}

// validSNSTopicARN checks the shape arn:<partition>:sns:<region>:<account>:<name>.
func validSNSTopicARN(s string) bool {
	parts := strings.Split(s, ":")
	return len(parts) == 6 && parts[0] == "arn" && strings.HasPrefix(parts[1], "aws") &&
		parts[2] == "sns" && parts[3] != "" && parts[4] != "" && parts[5] != ""
}
