package provision

import (
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// PolicyDocument is an IAM or resource policy.
type PolicyDocument struct {
	Version   string      `json:"Version"`
	ID        string      `json:"Id,omitempty"`
	Statement []Statement `json:"Statement"`
}

// Statement is one policy statement.
type Statement struct {
	Sid       string                           `json:"Sid,omitempty"`
	Effect    string                           `json:"Effect"`
	Principal map[string]StringList            `json:"Principal,omitempty"`
	Action    StringList                       `json:"Action"`
	Resource  StringList                       `json:"Resource"`
	Condition map[string]map[string]StringList `json:"Condition,omitempty"`
}

// StringList is a policy value that is either one string or a list of
// strings. It marshals a single element as a plain string.
type StringList []string

// MarshalJSON implements json.Marshaler.
func (l StringList) MarshalJSON() ([]byte, error) {
	if len(l) == 1 {
		return json.Marshal(l[0])
	}
	return json.Marshal([]string(l))
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

const policyVersion = "2012-10-17"

// SenderPolicy is the inline policy of mailroom-sender (PRD §11): sending
// through the identity and the configuration set, reading the account and
// the identity (for doctor), listing its topic's subscriptions,
// subscribing only Mailroom's own webhook (webhookURL, e.g.
// https://newsletter.example.com/hooks/ses) to it and, when a bucket is
// given, writing and listing backups.
//
// The topic's events carry every recipient address, and a subscription
// outlives the key that made it: without the endpoint condition, anyone
// holding a leaked sending key could subscribe their own endpoint and keep
// receiving them after the key is rotated.
func SenderPolicy(identityARN, configSetARN, topicARN, bucketARN, webhookURL string) PolicyDocument {
	p := PolicyDocument{
		Version: policyVersion,
		Statement: []Statement{
			{Sid: "SendEmail", Effect: "Allow", Action: StringList{"ses:SendEmail", "ses:SendRawEmail"}, Resource: StringList{identityARN, configSetARN}},
			{Sid: "ReadAccount", Effect: "Allow", Action: StringList{"ses:GetAccount"}, Resource: StringList{"*"}},
			{Sid: "ReadIdentity", Effect: "Allow", Action: StringList{"ses:GetEmailIdentity"}, Resource: StringList{identityARN}},
			{Sid: "ListTopicSubscriptions", Effect: "Allow", Action: StringList{"sns:ListSubscriptionsByTopic"}, Resource: StringList{topicARN}},
			{
				Sid: "SubscribeWebhook", Effect: "Allow", Action: StringList{"sns:Subscribe"}, Resource: StringList{topicARN},
				Condition: map[string]map[string]StringList{"StringEquals": {
					"sns:Protocol": {webhookProtocol(webhookURL)},
					"sns:Endpoint": {webhookURL},
				}},
			},
		},
	}
	if bucketARN != "" {
		p.Statement = append(p.Statement,
			Statement{Sid: "ListBackups", Effect: "Allow", Action: StringList{"s3:ListBucket"}, Resource: StringList{bucketARN}},
			Statement{Sid: "WriteBackups", Effect: "Allow", Action: StringList{"s3:PutObject"}, Resource: StringList{bucketARN + "/*"}},
		)
	}
	return p
}

// webhookProtocol is the SNS protocol of the webhook URL: http for an
// http:// URL (HTTP_ONLY installs behind a proxy), else https.
func webhookProtocol(u string) string {
	if strings.HasPrefix(strings.ToLower(u), "http://") {
		return "http"
	}
	return "https"
}

// WebhookURL is the SNS endpoint Mailroom subscribes: BASE_URL (or
// https://APP_SUBDOMAIN.DOMAIN) + /hooks/ses, as the server computes it.
func WebhookURL(baseURL, appSubdomain, domain string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://" + appSubdomain + "." + domain
	} else if u, err := url.Parse(base); err == nil && u.Host != "" {
		base = u.Scheme + "://" + strings.ToLower(u.Host)
	}
	return base + WebhookPath
}

// TopicPolicy lets SES publish to the topic, and only on behalf of this
// account's configuration set.
func TopicPolicy(topicARN, account, configSetARN string) PolicyDocument {
	return PolicyDocument{
		Version: policyVersion,
		ID:      TopicName + "-policy",
		Statement: []Statement{{
			Sid:       "AllowSESPublish",
			Effect:    "Allow",
			Principal: map[string]StringList{"Service": {"ses.amazonaws.com"}},
			Action:    StringList{"sns:Publish"},
			Resource:  StringList{topicARN},
			Condition: map[string]map[string]StringList{
				"StringEquals": {
					"AWS:SourceAccount": {account},
					"AWS:SourceArn":     {configSetARN},
				},
			},
		}},
	}
}

// JSON returns the compact JSON form of the policy.
func (p PolicyDocument) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		// PolicyDocument holds only strings, maps and slices.
		panic("provision: marshal policy: " + err.Error())
	}
	return string(b)
}

// policyMatches reports whether the policy JSON doc is semantically equal to
// want: statement order, list order and single-string vs list forms are
// ignored. A document that does not parse never matches.
func policyMatches(doc string, want PolicyDocument) bool {
	var got PolicyDocument
	if err := json.Unmarshal([]byte(doc), &got); err != nil {
		return false
	}
	return reflect.DeepEqual(canonicalPolicy(got), canonicalPolicy(want))
}

func canonicalPolicy(p PolicyDocument) PolicyDocument {
	out := PolicyDocument{Version: p.Version, ID: p.ID}
	for _, s := range p.Statement {
		c := Statement{Sid: s.Sid, Effect: s.Effect, Action: sortedList(s.Action), Resource: sortedList(s.Resource)}
		if len(s.Principal) > 0 {
			c.Principal = map[string]StringList{}
			for k, v := range s.Principal {
				c.Principal[k] = sortedList(v)
			}
		}
		if len(s.Condition) > 0 {
			c.Condition = map[string]map[string]StringList{}
			for op, kv := range s.Condition {
				c.Condition[op] = map[string]StringList{}
				for k, v := range kv {
					c.Condition[op][k] = sortedList(v)
				}
			}
		}
		out.Statement = append(out.Statement, c)
	}
	sort.SliceStable(out.Statement, func(i, j int) bool { return out.Statement[i].Sid < out.Statement[j].Sid })
	return out
}

func sortedList(l StringList) StringList {
	if len(l) == 0 {
		return nil
	}
	c := slices.Clone(l)
	slices.Sort(c)
	return c
}
