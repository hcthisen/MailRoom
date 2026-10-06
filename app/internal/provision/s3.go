package provision

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Error codes S3 answers when a bucket has no such configuration yet.
var (
	s3NoPublicAccessBlock = []string{"NoSuchPublicAccessBlockConfiguration"}
	s3NoEncryption        = []string{"ServerSideEncryptionConfigurationNotFoundError"}
	s3NoLifecycle         = []string{"NoSuchLifecycleConfiguration"}
	// s3AccessDenied is answered when the setup user may not read a bucket
	// setting. Guide step 2.6 grants only the Put* actions, so the setting
	// is then written without comparing it first.
	s3AccessDenied = []string{"AccessDenied", "AccessDeniedException"}
)

// bucketState is what a bucket-setting step knows about the current value.
type bucketState int

const (
	stateNew     bucketState = iota // the bucket was created in this run
	stateMissing                    // S3 says the setting is not set
	stateUnread                     // the setup user may not read it
	stateRead                       // the setting was read
)

// action is the step action for writing a setting in this state.
func (s bucketState) action() Action {
	if s == stateNew || s == stateMissing {
		return ActionCreated
	}
	return ActionUpdated
}

// suffix explains a blind write in the step detail.
func (s bucketState) suffix(readAction string) string {
	if s != stateUnread {
		return ""
	}
	return fmt.Sprintf(" (written without comparing: the setup user lacks %s)", readAction)
}

// readState classifies the error of a Get* call; other errors are returned.
func readState(err error, missing []string, op string) (bucketState, error) {
	switch {
	case err == nil:
		return stateRead, nil
	case isAPIError(err, missing...):
		return stateMissing, nil
	case isAPIError(err, s3AccessDenied...):
		return stateUnread, nil
	default:
		return 0, fmt.Errorf("%s: %w", op, err)
	}
}

// bucket ensures the private, encrypted, versioned backup bucket whose
// backups/ objects (and their overwritten versions) expire after
// BackupRetentionDays. A bucket created in this run is configured without
// reading its settings first. Versioning keeps what a leaked sending key
// (which may put objects) overwrites: the backups and keys/secret.key can
// be recovered, because the sending user may not delete versions.
func (r *run) bucket(ctx context.Context) error {
	if !r.in.BackupBucket {
		return nil
	}
	name := BackupBucketName(r.res.AccountID, r.c.Region)
	r.bucketName = name
	r.res.BackupBucket = name
	b := aws.String(name)
	owner := aws.String(r.res.AccountID)

	created := false
	_, err := r.c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: b, ExpectedBucketOwner: owner})
	switch {
	case err == nil:
		r.step("s3 bucket", ActionUnchanged, name)
	case isAPIError(err, s3NotFound...):
		// No ObjectOwnership: setting it needs s3:PutBucketOwnershipControls,
		// and BucketOwnerEnforced is already the default for new buckets.
		in := &s3.CreateBucketInput{Bucket: b}
		if r.c.Region != "us-east-1" {
			in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(r.c.Region),
			}
		}
		if _, err := r.c.S3.CreateBucket(ctx, in); err != nil {
			return fmt.Errorf("s3 create-bucket %s: %w", name, err)
		}
		created = true
		r.step("s3 bucket", ActionCreated, name)
	default:
		return fmt.Errorf("s3 head-bucket %s (it may belong to another account): %w", name, err)
	}

	for _, f := range []func(context.Context, *string, *string, bool) error{
		r.bucketPublicAccessBlock,
		r.bucketEncryption,
		r.bucketVersioning,
		r.bucketLifecycle,
	} {
		if err := f(ctx, b, owner, created); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) bucketPublicAccessBlock(ctx context.Context, b, owner *string, created bool) error {
	const detail = "all public access blocked"
	state := stateNew
	if !created {
		out, err := r.c.S3.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b, ExpectedBucketOwner: owner})
		if state, err = readState(err, s3NoPublicAccessBlock, "s3 get-public-access-block"); err != nil {
			return err
		}
		if state == stateRead && fullPublicAccessBlock(out.PublicAccessBlockConfiguration) {
			r.step("s3 public access block", ActionUnchanged, detail)
			return nil
		}
	}
	_, err := r.c.S3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket:              b,
		ExpectedBucketOwner: owner,
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put-public-access-block: %w", err)
	}
	r.step("s3 public access block", state.action(), detail+state.suffix("s3:GetBucketPublicAccessBlock"))
	return nil
}

func fullPublicAccessBlock(c *s3types.PublicAccessBlockConfiguration) bool {
	return c != nil && aws.ToBool(c.BlockPublicAcls) && aws.ToBool(c.IgnorePublicAcls) &&
		aws.ToBool(c.BlockPublicPolicy) && aws.ToBool(c.RestrictPublicBuckets)
}

func (r *run) bucketEncryption(ctx context.Context, b, owner *string, created bool) error {
	const detail = "SSE AES256"
	state := stateNew
	if !created {
		out, err := r.c.S3.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: b, ExpectedBucketOwner: owner})
		if state, err = readState(err, s3NoEncryption, "s3 get-bucket-encryption"); err != nil {
			return err
		}
		if state == stateRead && aes256Only(out.ServerSideEncryptionConfiguration) {
			r.step("s3 encryption", ActionUnchanged, detail)
			return nil
		}
	}
	_, err := r.c.S3.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket:              b,
		ExpectedBucketOwner: owner,
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{SSEAlgorithm: s3types.ServerSideEncryptionAes256},
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put-bucket-encryption: %w", err)
	}
	r.step("s3 encryption", state.action(), detail+state.suffix("s3:GetEncryptionConfiguration"))
	return nil
}

func aes256Only(c *s3types.ServerSideEncryptionConfiguration) bool {
	if c == nil || len(c.Rules) != 1 {
		return false
	}
	d := c.Rules[0].ApplyServerSideEncryptionByDefault
	return d != nil && d.SSEAlgorithm == s3types.ServerSideEncryptionAes256
}

// bucketVersioning turns on versioning. Setups whose setup user may not
// (guide step 2.6 before s3:PutBucketVersioning was added) get a warning
// instead of a failure, so a key rotation still works.
func (r *run) bucketVersioning(ctx context.Context, b, owner *string, created bool) error {
	const detail = "versioning enabled (overwritten backups can be recovered)"
	state := stateNew
	if !created {
		out, err := r.c.S3.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: b, ExpectedBucketOwner: owner})
		if state, err = readState(err, nil, "s3 get-bucket-versioning"); err != nil {
			return err
		}
		if state == stateRead && out.Status == s3types.BucketVersioningStatusEnabled {
			r.step("s3 versioning", ActionUnchanged, detail)
			return nil
		}
		if state == stateRead && out.Status == "" {
			state = stateMissing
		}
	}
	_, err := r.c.S3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  b,
		ExpectedBucketOwner:     owner,
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if isAPIError(err, s3AccessDenied...) {
		r.res.Warnings = append(r.res.Warnings, "The backup bucket's versioning could not be turned on: the setup user lacks "+
			"s3:PutBucketVersioning. Add s3:GetBucketVersioning and s3:PutBucketVersioning to its allow-mailroom-setup policy "+
			"(guide step 2.6) and run provision ses again, so a leaked sending key can't overwrite the off-site backups for good.")
		r.step("s3 versioning", ActionUnchanged, "not enabled: the setup user lacks s3:PutBucketVersioning")
		return nil
	}
	if err != nil {
		return fmt.Errorf("s3 put-bucket-versioning: %w", err)
	}
	r.step("s3 versioning", state.action(), detail+state.suffix("s3:GetBucketVersioning"))
	return nil
}

// bucketLifecycle ensures Mailroom's expiry rule, keeping any other rules
// it can read. When the setup user may not read the configuration, it is
// replaced by Mailroom's rule alone (the bucket is Mailroom's own).
func (r *run) bucketLifecycle(ctx context.Context, b, owner *string, created bool) error {
	detail := fmt.Sprintf("expire %s (and overwritten versions) after %d days", BackupPrefix, BackupRetentionDays)
	state := stateNew
	var rules []s3types.LifecycleRule
	if !created {
		out, err := r.c.S3.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b, ExpectedBucketOwner: owner})
		if state, err = readState(err, s3NoLifecycle, "s3 get-bucket-lifecycle-configuration"); err != nil {
			return err
		}
		if state == stateRead {
			rules = out.Rules
		}
	}
	i := slices.IndexFunc(rules, func(l s3types.LifecycleRule) bool { return aws.ToString(l.ID) == LifecycleRuleID })
	if i >= 0 && lifecycleRuleOK(rules[i]) {
		r.step("s3 lifecycle", ActionUnchanged, detail)
		return nil
	}
	want := s3types.LifecycleRule{
		ID:         aws.String(LifecycleRuleID),
		Status:     s3types.ExpirationStatusEnabled,
		Filter:     &s3types.LifecycleRuleFilter{Prefix: aws.String(BackupPrefix)},
		Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(BackupRetentionDays)},
		NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{
			NoncurrentDays: aws.Int32(BackupRetentionDays),
		},
	}
	next := slices.Clone(rules)
	if i >= 0 {
		next[i] = want
	} else {
		next = append(next, want)
	}
	_, err := r.c.S3.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 b,
		ExpectedBucketOwner:    owner,
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: next},
	})
	if err != nil {
		return fmt.Errorf("s3 put-bucket-lifecycle-configuration: %w", err)
	}
	action := state.action()
	if state == stateRead && i < 0 {
		// Other rules exist, but Mailroom's was missing.
		action = ActionCreated
	}
	r.step("s3 lifecycle", action, detail+state.suffix("s3:GetLifecycleConfiguration"))
	return nil
}

func lifecycleRuleOK(l s3types.LifecycleRule) bool {
	return l.Status == s3types.ExpirationStatusEnabled &&
		l.Filter != nil && aws.ToString(l.Filter.Prefix) == BackupPrefix && l.Filter.And == nil && l.Filter.Tag == nil &&
		l.Expiration != nil && aws.ToInt32(l.Expiration.Days) == BackupRetentionDays &&
		l.NoncurrentVersionExpiration != nil && aws.ToInt32(l.NoncurrentVersionExpiration.NoncurrentDays) == BackupRetentionDays
}
