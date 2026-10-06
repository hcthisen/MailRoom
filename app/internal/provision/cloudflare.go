package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultCloudflareBaseURL is the Cloudflare API v4 endpoint.
const DefaultCloudflareBaseURL = "https://api.cloudflare.com/client/v4"

// maxCloudflareBody caps how much of a Cloudflare response is read.
const maxCloudflareBody = 16 << 20

// Cloudflare is a minimal Cloudflare DNS client. It can only read zones
// and records and create records: there is deliberately no update or
// delete (L-3).
type Cloudflare struct {
	// BaseURL defaults to DefaultCloudflareBaseURL.
	BaseURL string
	// Token is the API token (Zone:Read and DNS:Edit on the zone).
	Token string
	// HTTP defaults to a client with a 30 s timeout.
	HTTP *http.Client
}

// Zone is a Cloudflare zone.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DNSRecord is a record as Cloudflare stores it.
type DNSRecord struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl,omitempty"`
	Priority *int   `json:"priority,omitempty"`
	Proxied  *bool  `json:"proxied,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// CloudflareError is an unsuccessful Cloudflare API answer.
type CloudflareError struct {
	Status   int
	Messages []string
}

// Error implements error.
func (e *CloudflareError) Error() string {
	msg := strings.Join(e.Messages, "; ")
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("cloudflare: HTTP %d: %s", e.Status, msg)
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Cloudflare) do(ctx context.Context, method, path string, query url.Values, body any, out any) (*cfEnvelope, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultCloudflareBaseURL
	}
	u := strings.TrimSuffix(base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("cloudflare: encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudflareBody))
	if err != nil {
		return nil, fmt.Errorf("cloudflare: read answer: %w", err)
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 300 {
			return nil, &CloudflareError{Status: resp.StatusCode}
		}
		return nil, fmt.Errorf("cloudflare: %s %s: invalid JSON answer", method, path)
	}
	if resp.StatusCode >= 300 || !env.Success {
		ce := &CloudflareError{Status: resp.StatusCode}
		for _, e := range env.Errors {
			ce.Messages = append(ce.Messages, fmt.Sprintf("%s (code %d)", e.Message, e.Code))
		}
		return nil, ce
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, fmt.Errorf("cloudflare: %s %s: unexpected answer: %w", method, path, err)
		}
	}
	return &env, nil
}

// FindZone returns the zone that holds domain: the zone named domain, or
// else the closest parent zone.
func (c *Cloudflare) FindZone(ctx context.Context, domain string) (Zone, error) {
	domain = normalizeDomain(domain)
	labels := strings.Split(domain, ".")
	for i := 0; i+2 <= len(labels); i++ {
		name := strings.Join(labels[i:], ".")
		var zones []Zone
		if _, err := c.do(ctx, http.MethodGet, "/zones", url.Values{"name": {name}}, nil, &zones); err != nil {
			return Zone{}, err
		}
		for _, z := range zones {
			if strings.EqualFold(z.Name, name) {
				return z, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("cloudflare: no zone for %s is visible to this token (it needs Zone:Read and DNS:Edit on the zone)", domain)
}

// ListRecords returns every DNS record of the zone.
func (c *Cloudflare) ListRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	var all []DNSRecord
	for page := 1; ; page++ {
		var recs []DNSRecord
		env, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records",
			url.Values{"page": {strconv.Itoa(page)}, "per_page": {"500"}}, nil, &recs)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages || len(recs) == 0 {
			return all, nil
		}
	}
}

// CreateRecord creates one record and returns it as stored.
func (c *Cloudflare) CreateRecord(ctx context.Context, zoneID string, r DNSRecord) (DNSRecord, error) {
	if r.Type == "" || r.Name == "" || r.Content == "" {
		return DNSRecord{}, errors.New("cloudflare: a record needs a type, name and content")
	}
	r.ID = ""
	var out DNSRecord
	if _, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, r, &out); err != nil {
		return DNSRecord{}, fmt.Errorf("create %s %s: %w", r.Type, r.Name, err)
	}
	return out, nil
}
