package core

import "strings"

// regionalDKIM are the SES regions whose Easy DKIM records point at a
// region-specific zone instead of dkim.amazonses.com.
var regionalDKIM = map[string]bool{
	"af-south-1":     true,
	"ap-northeast-3": true,
	"ap-southeast-3": true,
	"eu-south-1":     true,
	"il-central-1":   true,
	"us-gov-east-1":  true,
	"us-gov-west-1":  true,
}

// SESDKIMDomain is the zone the Easy DKIM CNAME records of an SES
// identity point into (<token>._domainkey.DOMAIN -> <token>.<zone>): the
// identity's SigningHostedZone when SES reports one, else the region's
// default. provision prints and creates the records with it, and the
// doctor checks them against it.
func SESDKIMDomain(hostedZone, region string) string {
	if z := strings.TrimSuffix(strings.TrimSpace(hostedZone), "."); z != "" {
		return strings.ToLower(z)
	}
	if regionalDKIM[region] {
		return "dkim." + region + ".amazonses.com"
	}
	return "dkim.amazonses.com"
}
