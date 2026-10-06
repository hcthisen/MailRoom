package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"mailroom/internal/core"
)

// Backup naming and retention (O-3).
const (
	// BackupPrefix starts the names of nightly, on-demand and CLI backups:
	// mailroom-20260105T023000Z.db (UTC).
	BackupPrefix = "mailroom-"
	// PreRestorePrefix starts the safety backup Restore takes first.
	PreRestorePrefix = "pre-restore-"
	// BackupStampFormat is the UTC timestamp in backup names (the same one
	// `mailroom migrate` uses for its pre-migrate-v<N>-<stamp>.db backups).
	BackupStampFormat = "20060102T150405Z"
	// BackupRetention is how long local backups are kept.
	BackupRetention = 14 * 24 * time.Hour
	// S3KeyPrefix is the key prefix of uploaded backups (the bucket's
	// 30-day lifecycle rule applies to it).
	S3KeyPrefix = "backups/"
	// S3SecretKey is the object key of the HMAC key file (<data>/secret.key)
	// uploaded next to every backup. It lies outside S3KeyPrefix, so the
	// bucket's 30-day lifecycle rule never expires it; each upload replaces
	// it. Restoring a backup on a new server needs it: without the key, the
	// unsubscribe, click, open and confirmation links in emails already
	// delivered stop working.
	S3SecretKey = "keys/secret.key"
)

// rxBackupName matches every backup file name this package lists, serves,
// prunes or restores: lowercase letters, digits and dashes, then the UTC
// stamp and ".db". It excludes path separators, dots and anything else, so
// a name can never leave the backups directory.
var rxBackupName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}-([0-9]{8}T[0-9]{6}Z)\.db$`)

// ValidBackupName reports whether name is a well-formed backup file name.
func ValidBackupName(name string) bool { return rxBackupName.MatchString(name) }

// backupTime returns the creation time encoded in a valid backup name.
func backupTime(name string) (time.Time, bool) {
	m := rxBackupName.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(BackupStampFormat, m[1])
	return t.UTC(), err == nil
}

// backupName returns the name of a backup taken at t with prefix.
func backupName(prefix string, t time.Time) string {
	return prefix + t.UTC().Format(BackupStampFormat) + ".db"
}

// BackupInfo describes one local backup file.
type BackupInfo struct {
	Name      string    `json:"name" doc:"File name in the backups directory; use it to download the backup" example:"mailroom-20260105T023000Z.db"`
	SizeBytes int64     `json:"size_bytes" doc:"File size in bytes" example:"1048576"`
	CreatedAt time.Time `json:"created_at" doc:"When the backup was taken (from its name)"`

	// modTime is the file's modification time; it stands in for CreatedAt
	// when the name's stamp lies in the future (see effectiveTime).
	modTime time.Time
}

// backupClockTolerance is how far a backup's name stamp may lie in the
// future before it is treated as wrong (the clock was ahead when it was
// taken, or the file was copied in).
const backupClockTolerance = 5 * time.Minute

// future reports whether b's name stamp lies implausibly far after now.
func (b BackupInfo) future(now time.Time) bool {
	return b.CreatedAt.After(now.Add(backupClockTolerance))
}

// effectiveTime is when b was taken for pruning: the name stamp, or the
// file's modification time when the stamp is in the future, so a wrongly
// dated file is still deleted after the retention period.
func (b BackupInfo) effectiveTime(now time.Time) time.Time {
	if b.future(now) && !b.modTime.IsZero() {
		return b.modTime
	}
	return b.CreatedAt
}

// latestScheduled returns the newest nightly, on-demand or CLI backup
// (BackupPrefix) whose stamp isn't in the future, and the names of those
// that are. Safety copies (pre-restore-, pre-migrate-) don't count: they
// copy a database that was about to be replaced or migrated.
func latestScheduled(list []BackupInfo, now time.Time) (latest BackupInfo, found bool, future []string) {
	for _, b := range list {
		if !strings.HasPrefix(b.Name, BackupPrefix) {
			continue
		}
		if b.future(now) {
			future = append(future, b.Name)
			continue
		}
		if !found {
			latest, found = b, true // list is newest first
		}
	}
	return latest, found, future
}

// BackupResult is what a backup run did.
type BackupResult struct {
	Backup BackupInfo `json:"backup"`
	// Path is the backup's local path.
	Path string `json:"path" doc:"Local path of the backup file" example:"/data/backups/mailroom-20260105T023000Z.db"`
	// S3Key is set when the backup was uploaded to BACKUP_BUCKET.
	S3Key string `json:"s3_key,omitempty" doc:"S3 object key, when uploaded to BACKUP_BUCKET" example:"backups/mailroom-20260105T023000Z.db"`
	// SecretKeyS3Key is set when the HMAC key file was uploaded with it.
	SecretKeyS3Key string `json:"secret_key_s3_key,omitempty" doc:"S3 object key of the uploaded secret.key (the key that signs unsubscribe and tracking links), when the install uses the generated key file rather than MAILROOM_SECRET" example:"keys/secret.key"`
	// Pruned lists the local backups deleted for age.
	Pruned []string `json:"pruned" doc:"Local backups older than 14 days that were deleted"`
}

// ListBackups returns the local backups, newest first. A missing backups
// directory is an empty list.
func (s *Service) ListBackups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(s.cfg.BackupsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("system: list backups: %w", err)
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		at, ok := backupTime(e.Name())
		if !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		out = append(out, BackupInfo{Name: e.Name(), SizeBytes: fi.Size(), CreatedAt: at, modTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// Backup takes a backup now: VACUUM INTO <data>/backups/mailroom-<UTC
// stamp>.db (mode 0600), then deletes local backups older than 14 days and,
// when BACKUP_BUCKET is set, uploads the file to S3 under backups/<name>
// with SSE-S3 (AES256) encryption, and the generated HMAC key file with it
// (S3SecretKey). It works offline (CLI) as well as in the
// server. When only the upload fails, the result describes the local backup
// and the error says why the upload failed. It raises no alert; the worker
// and job do.
//
// Backup names have one-second resolution. A second request in the same
// second as this process's previous backup returns that backup (retrying
// its upload if that failed) instead of failing; a same-named file this
// process didn't write wraps core.ErrConflict.
func (s *Service) Backup(ctx context.Context) (BackupResult, error) {
	if s.db == nil {
		return BackupResult{}, errNoDB
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	now := s.clock.Now()
	name := backupName(BackupPrefix, now)
	path := filepath.Join(s.cfg.BackupsDir(), name)
	if _, err := os.Stat(path); err == nil {
		if s.lastBackup.Backup.Name == name {
			return s.reuseLastBackup(ctx)
		}
		return BackupResult{}, fmt.Errorf("backup %s was taken less than a second ago by another process: %w", name, core.ErrConflict)
	}
	if err := s.db.Backup(ctx, path); err != nil {
		return BackupResult{}, fmt.Errorf("system: backup: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return BackupResult{}, fmt.Errorf("system: backup: %w", err)
	}
	res := BackupResult{Backup: BackupInfo{Name: name, SizeBytes: fi.Size(), CreatedAt: now.UTC().Truncate(time.Second)}, Path: path}
	s.log.Info("system: backup written", "name", name, "bytes", fi.Size())

	pruned, err := s.pruneBackups(now)
	res.Pruned = pruned
	if err != nil {
		s.log.Warn("system: pruning old backups failed", "err", err)
	}
	s.lastBackup = res
	return s.uploadIfConfigured(ctx)
}

// reuseLastBackup returns the backup this process took earlier in the same
// second, uploading it when that upload is still missing. backupMu is held.
func (s *Service) reuseLastBackup(ctx context.Context) (BackupResult, error) {
	s.log.Info("system: backup requested again within the same second; reusing it", "name", s.lastBackup.Backup.Name)
	s.lastBackup.Pruned = []string{}
	return s.uploadIfConfigured(ctx)
}

// uploadIfConfigured uploads s.lastBackup to BACKUP_BUCKET unless uploads
// are off or it is already uploaded, then the HMAC key file (S3SecretKey)
// unless that is already done for this backup, and returns it. backupMu is
// held.
func (s *Service) uploadIfConfigured(ctx context.Context) (BackupResult, error) {
	if s.cfg.BackupBucket == "" {
		return s.lastBackup, nil
	}
	if s.lastBackup.S3Key == "" {
		name := s.lastBackup.Backup.Name
		key, err := s.uploadBackup(ctx, s.lastBackup.Path, name)
		if err != nil {
			return s.lastBackup, fmt.Errorf("system: upload %s to s3://%s: %w", name, s.cfg.BackupBucket, err)
		}
		s.lastBackup.S3Key = key
		s.log.Info("system: backup uploaded", "bucket", s.cfg.BackupBucket, "key", key)
	}
	if s.lastBackup.SecretKeyS3Key == "" {
		key, err := s.uploadSecretKey(ctx)
		if err != nil {
			return s.lastBackup, fmt.Errorf("system: upload the secret key to s3://%s/%s: %w", s.cfg.BackupBucket, S3SecretKey, err)
		}
		s.lastBackup.SecretKeyS3Key = key
	}
	return s.lastBackup, nil
}

// uploadSecretKey puts <data>/secret.key into BACKUP_BUCKET at S3SecretKey
// when it holds the key this process signs with, and returns the object
// key. With MAILROOM_SECRET set (the key lives in the env file, which the
// operator keeps) or no key file, there is nothing to upload: "", nil.
func (s *Service) uploadSecretKey(ctx context.Context) (string, error) {
	path := s.cfg.SecretKeyPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) != string(s.cfg.Secret) {
		return "", nil
	}
	if s.s3 == nil {
		return "", errAWSNotConfigured
	}
	key, bucket, size, contentType := S3SecretKey, s.cfg.BackupBucket, int64(len(b)), "text/plain"
	if _, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               &bucket,
		Key:                  &key,
		Body:                 bytes.NewReader(b),
		ContentLength:        &size,
		ContentType:          &contentType,
		ServerSideEncryption: s3types.ServerSideEncryptionAes256,
	}); err != nil {
		return "", err
	}
	s.log.Info("system: secret key uploaded", "bucket", bucket, "key", key)
	return key, nil
}

// pruneBackups deletes local backups older than BackupRetention (by the
// time in their name, or their modification time when that lies in the
// future) and returns their names.
func (s *Service) pruneBackups(now time.Time) ([]string, error) {
	list, err := s.ListBackups()
	if err != nil {
		return []string{}, err
	}
	pruned := []string{}
	var errs []error
	cutoff := now.Add(-BackupRetention)
	for _, b := range list {
		if !b.effectiveTime(now).Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.BackupsDir(), b.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		pruned = append(pruned, b.Name)
	}
	return pruned, errors.Join(errs...)
}

// uploadBackup puts the backup file into BACKUP_BUCKET.
func (s *Service) uploadBackup(ctx context.Context, path, name string) (string, error) {
	if s.s3 == nil {
		return "", errAWSNotConfigured
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	key := S3KeyPrefix + name
	size := fi.Size()
	bucket := s.cfg.BackupBucket
	contentType := backupContentType
	_, err = s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:               &bucket,
		Key:                  &key,
		Body:                 f,
		ContentLength:        &size,
		ContentType:          &contentType,
		ServerSideEncryption: s3types.ServerSideEncryptionAes256,
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

// backupContentType is the media type of SQLite database files.
const backupContentType = "application/vnd.sqlite3"

// OpenBackup opens the local backup name for reading. Names that aren't
// valid backup names are refused with a validation error before touching
// the file system, and the file is opened through an os.Root confined to the
// backups directory, so no name can reach a file outside it. Unknown names
// wrap core.ErrNotFound.
func (s *Service) OpenBackup(name string) (*os.File, BackupInfo, error) {
	if !ValidBackupName(name) {
		return nil, BackupInfo{}, core.Invalid("name", "is not a backup file name (e.g. mailroom-20260105T023000Z.db)")
	}
	notFound := fmt.Errorf("backup %s: %w", name, core.ErrNotFound)
	root, err := os.OpenRoot(s.cfg.BackupsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, BackupInfo{}, notFound
	}
	if err != nil {
		return nil, BackupInfo{}, fmt.Errorf("system: open backups directory: %w", err)
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		// Missing, or a symlink escaping the directory (refused by os.Root).
		return nil, BackupInfo{}, notFound
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, BackupInfo{}, notFound
	}
	at, _ := backupTime(name)
	return f, BackupInfo{Name: name, SizeBytes: fi.Size(), CreatedAt: at}, nil
}
