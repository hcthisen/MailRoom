package provision

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// ErrKeysNotInEnvFile means the sending user already has access keys but
// the env file does not hold one of them. AWS never shows a secret key
// again, so the only way forward is --rotate-keys.
var ErrKeysNotInEnvFile = errors.New("the sending user's access key is not in the env file")

// maxAccessKeys is IAM's limit of access keys per user.
const maxAccessKeys = 2

func (r *run) iamUser(ctx context.Context) error {
	_, err := r.c.IAM.GetUser(ctx, &iam.GetUserInput{UserName: aws.String(SenderUserName)})
	switch {
	case err == nil:
		r.step("iam user", ActionUnchanged, SenderUserName)
		return nil
	case !isAPIError(err, iamNotFound...):
		return fmt.Errorf("iam get-user %s: %w", SenderUserName, err)
	}
	_, err = r.c.IAM.CreateUser(ctx, &iam.CreateUserInput{
		UserName: aws.String(SenderUserName),
		Tags:     []iamtypes.Tag{{Key: aws.String("managed-by"), Value: aws.String("mailroom")}},
	})
	if err != nil {
		return fmt.Errorf("iam create-user %s: %w", SenderUserName, err)
	}
	r.step("iam user", ActionCreated, SenderUserName)
	return nil
}

// senderPolicy is the policy the sending user must have in this run.
func (r *run) senderPolicy() PolicyDocument {
	bucketARN := ""
	if r.bucketName != "" {
		bucketARN = r.arns.bucket(r.bucketName)
	}
	inboundARN := ""
	if r.receive {
		inboundARN = r.arns.bucket(r.res.InboundBucket)
	}
	return SenderPolicy(r.res.IdentityARN, r.res.ConfigurationSetARN, r.res.TopicARN, bucketARN, r.in.WebhookURL, inboundARN)
}

func (r *run) iamPolicy(ctx context.Context) error {
	want := r.senderPolicy()
	out, err := r.c.IAM.GetUserPolicy(ctx, &iam.GetUserPolicyInput{
		UserName:   aws.String(SenderUserName),
		PolicyName: aws.String(SenderPolicyName),
	})
	missing := isAPIError(err, iamNotFound...)
	if err != nil && !missing {
		return fmt.Errorf("iam get-user-policy %s: %w", SenderPolicyName, err)
	}
	if !missing && policyMatches(decodePolicy(aws.ToString(out.PolicyDocument)), want) {
		r.step("iam policy", ActionUnchanged, SenderPolicyName)
		return nil
	}
	_, err = r.c.IAM.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName:       aws.String(SenderUserName),
		PolicyName:     aws.String(SenderPolicyName),
		PolicyDocument: aws.String(want.JSON()),
	})
	if err != nil {
		return fmt.Errorf("iam put-user-policy %s: %w", SenderPolicyName, err)
	}
	r.step("iam policy", createdOrUpdated(missing), SenderPolicyName)
	return nil
}

// decodePolicy undoes the URL encoding IAM applies to returned policies.
func decodePolicy(doc string) string {
	if dec, err := url.PathUnescape(doc); err == nil {
		return dec
	}
	return doc
}

func (r *run) listAccessKeys(ctx context.Context) ([]iamtypes.AccessKeyMetadata, error) {
	var keys []iamtypes.AccessKeyMetadata
	var marker *string
	for {
		out, err := r.c.IAM.ListAccessKeys(ctx, &iam.ListAccessKeysInput{UserName: aws.String(SenderUserName), Marker: marker})
		if err != nil {
			return nil, fmt.Errorf("iam list-access-keys: %w", err)
		}
		keys = append(keys, out.AccessKeyMetadata...)
		if !out.IsTruncated || out.Marker == nil {
			return keys, nil
		}
		marker = out.Marker
	}
}

// accessKeyAndEnv makes sure the env file holds a working key of the
// sending user plus the resource names, creating a key when the user has
// none and rotating it on request.
func (r *run) accessKeyAndEnv(ctx context.Context) error {
	keys, err := r.listAccessKeys(ctx)
	if err != nil {
		return err
	}
	env, err := ReadEnvFile(r.in.EnvFile)
	if err != nil {
		return err
	}
	envKeyID := env["AWS_ACCESS_KEY_ID"]
	set := map[string]string{
		"AWS_REGION":            r.c.Region,
		"SNS_TOPIC_ARN":         r.res.TopicARN,
		"SES_CONFIGURATION_SET": r.in.ConfigurationSet,
		"BACKUP_BUCKET":         r.bucketName,
		"INBOUND_BUCKET":        r.res.InboundBucket,
		"INBOUND_DOMAIN":        r.res.InboundDomain,
	}

	var created *iamtypes.AccessKey
	var old []string
	keyAction := ActionUnchanged
	switch {
	case len(keys) == 0:
		if created, err = r.createAccessKey(ctx); err != nil {
			return err
		}
		keyAction = ActionCreated
	case r.in.RotateKeys:
		if keys, err = r.makeRoomForKey(ctx, keys, envKeyID); err != nil {
			return err
		}
		if created, err = r.createAccessKey(ctx); err != nil {
			return err
		}
		for _, k := range keys {
			old = append(old, aws.ToString(k.AccessKeyId))
		}
		keyAction = ActionUpdated
	default:
		ids := keyIDs(keys)
		if envKeyID == "" || !slices.Contains(ids, envKeyID) || env["AWS_SECRET_ACCESS_KEY"] == "" {
			return fmt.Errorf("%w: IAM user %s already has access key %s, but %s has no matching AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. "+
				"AWS cannot show an existing secret again: run again with --rotate-keys to create a new key, write it to %s and delete the old one",
				ErrKeysNotInEnvFile, SenderUserName, strings.Join(ids, ", "), r.in.EnvFile, r.in.EnvFile)
		}
		r.res.AccessKeyID = envKeyID
	}
	if created != nil {
		set["AWS_ACCESS_KEY_ID"] = aws.ToString(created.AccessKeyId)
		set["AWS_SECRET_ACCESS_KEY"] = aws.ToString(created.SecretAccessKey)
		r.res.AccessKeyID = aws.ToString(created.AccessKeyId)
	}

	_, statErr := os.Stat(r.in.EnvFile)
	existed := statErr == nil
	changed, err := WriteEnvFile(r.in.EnvFile, set)
	if err != nil {
		if created != nil {
			// The new secret is lost with the failed write; do not leave an
			// unusable key behind.
			_, _ = r.c.IAM.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(SenderUserName), AccessKeyId: created.AccessKeyId})
		}
		return err
	}
	r.step("iam access key", keyAction, r.res.AccessKeyID)
	envAction := ActionUnchanged
	if changed {
		envAction = createdOrUpdated(!existed)
	}
	r.step("env file", envAction, r.in.EnvFile+" (mode 0600)")

	if len(old) > 0 {
		for _, id := range old {
			if _, err := r.c.IAM.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(SenderUserName), AccessKeyId: aws.String(id)}); err != nil {
				return fmt.Errorf("iam delete-access-key %s (the new key is already in the env file; delete the old key by hand): %w", id, err)
			}
		}
		r.step("iam old access keys", ActionUpdated, "deleted "+strings.Join(old, ", "))
	}
	return nil
}

func (r *run) createAccessKey(ctx context.Context) (*iamtypes.AccessKey, error) {
	out, err := r.c.IAM.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String(SenderUserName)})
	if err != nil {
		return nil, fmt.Errorf("iam create-access-key: %w", err)
	}
	if out.AccessKey == nil || aws.ToString(out.AccessKey.AccessKeyId) == "" || aws.ToString(out.AccessKey.SecretAccessKey) == "" {
		return nil, errors.New("iam create-access-key: no key in the answer")
	}
	return out.AccessKey, nil
}

// makeRoomForKey deletes keys the env file does not use (inactive first,
// then the oldest) until another key can be created, and returns the keys
// that remain.
func (r *run) makeRoomForKey(ctx context.Context, keys []iamtypes.AccessKeyMetadata, envKeyID string) ([]iamtypes.AccessKeyMetadata, error) {
	keys = slices.Clone(keys)
	for len(keys) >= maxAccessKeys {
		victim := -1
		for i, k := range keys {
			if aws.ToString(k.AccessKeyId) == envKeyID {
				continue
			}
			if victim < 0 || evictBefore(k, keys[victim]) {
				victim = i
			}
		}
		if victim < 0 {
			return nil, errors.New("iam: no access key can be deleted to make room for a new one")
		}
		id := aws.ToString(keys[victim].AccessKeyId)
		if _, err := r.c.IAM.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: aws.String(SenderUserName), AccessKeyId: aws.String(id)}); err != nil {
			return nil, fmt.Errorf("iam delete-access-key %s: %w", id, err)
		}
		r.step("iam unused access key", ActionUpdated, "deleted "+id+" (not in the env file) to make room")
		keys = slices.Delete(keys, victim, victim+1)
	}
	return keys, nil
}

// evictBefore orders keys for deletion: inactive before active, then older
// before newer.
func evictBefore(a, b iamtypes.AccessKeyMetadata) bool {
	aInactive, bInactive := a.Status == iamtypes.StatusTypeInactive, b.Status == iamtypes.StatusTypeInactive
	if aInactive != bInactive {
		return aInactive
	}
	return aws.ToTime(a.CreateDate).Before(aws.ToTime(b.CreateDate))
}

func keyIDs(keys []iamtypes.AccessKeyMetadata) []string {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, aws.ToString(k.AccessKeyId))
	}
	return ids
}
