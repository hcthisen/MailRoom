package provision

import (
	"fmt"
	"strings"
)

// Fixed names of the resources provisioning manages.
const (
	// DefaultConfigurationSet is the SES configuration set every send uses (D-3).
	DefaultConfigurationSet = "mailroom"
	// TopicName is the SNS topic SES publishes events to.
	TopicName = "mailroom-ses-events"
	// EventDestinationName is the configuration set's SNS event destination.
	EventDestinationName = "mailroom-sns"
	// SenderUserName is the least-privilege sending IAM user.
	SenderUserName = "mailroom-sender"
	// SenderPolicyName is the sending user's inline policy.
	SenderPolicyName = "mailroom-sender"
	// BackupPrefix is the key prefix of backups in the bucket.
	BackupPrefix = "backups/"
	// BackupRetentionDays is how long the lifecycle rule keeps backups.
	BackupRetentionDays = 30
	// WebhookPath is Mailroom's SNS endpoint (events.PathSESHook).
	WebhookPath = "/hooks/ses"
	// LifecycleRuleID names the bucket's expiry rule.
	LifecycleRuleID = "mailroom-expire-backups"
	// DefaultEnvFile is the server env file "provision ses" writes.
	DefaultEnvFile = "/opt/mailroom/.env"
)

// arns derives the ARNs of the managed resources from the account.
type arns struct {
	partition, region, account string
	domain, configSet          string
}

func (a arns) identity() string {
	return fmt.Sprintf("arn:%s:ses:%s:%s:identity/%s", a.partition, a.region, a.account, a.domain)
}

func (a arns) configurationSet() string {
	return fmt.Sprintf("arn:%s:ses:%s:%s:configuration-set/%s", a.partition, a.region, a.account, a.configSet)
}

func (a arns) topic() string {
	return fmt.Sprintf("arn:%s:sns:%s:%s:%s", a.partition, a.region, a.account, TopicName)
}

func (a arns) bucket(name string) string { return fmt.Sprintf("arn:%s:s3:::%s", a.partition, name) }

// partitionOf returns the partition of an ARN ("aws" if it cannot tell).
func partitionOf(arn string) string {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) >= 2 && parts[0] == "arn" && parts[1] != "" {
		return parts[1]
	}
	return "aws"
}

// BackupBucketName is the backup bucket of an account and region.
func BackupBucketName(account, region string) string {
	return "mailroom-backup-" + account + "-" + region
}

// RecreateHint tells the operator how a running server picks up the keys
// "provision ses" wrote. Compose reads env_file only when it creates a
// container, so "docker compose restart" keeps the old environment.
const RecreateHint = "If Mailroom is running, recreate its container so it reads the env file: " +
	"docker compose up -d (a plain docker compose restart keeps the old environment)."
