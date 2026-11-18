package databases

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/thekrauss/kubemanager/internal/core/configs"
)

var (
	retentionRe = regexp.MustCompile(`^[1-9][0-9]*[dwm]$`)
	bucketRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

const (
	defaultRetention = "7d"
	defaultSchedule  = "0 0 3 * * *" // chaque nuit à 3 h UTC (6 champs : secondes d'abord)
)

// BackupTargetFrom : destination des sauvegardes depuis la configuration ;
// nil sans endpoint ni bucket (sauvegardes désactivées).
func BackupTargetFrom(c configs.DatabaseBackupConfig) (*BackupTarget, error) {
	if strings.TrimSpace(c.Endpoint) == "" && strings.TrimSpace(c.Bucket) == "" {
		return nil, nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("databases.backup.endpoint: full URL expected (https://…)")
	}
	if !bucketRe.MatchString(c.Bucket) {
		return nil, errors.New("databases.backup.bucket: invalid bucket name")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("databases.backup: access_key and secret_key are required")
	}
	t := &BackupTarget{Endpoint: strings.TrimRight(c.Endpoint, "/"), Bucket: c.Bucket, Region: c.Region,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, Retention: c.Retention, Schedule: c.Schedule}
	if t.Retention == "" {
		t.Retention = defaultRetention
	}
	if !retentionRe.MatchString(t.Retention) {
		return nil, errors.New("databases.backup.retention: expected 7d, 4w, 3m…")
	}
	if t.Schedule == "" {
		t.Schedule = defaultSchedule
	}
	if len(strings.Fields(t.Schedule)) != 6 {
		return nil, errors.New("databases.backup.schedule: 6 fields expected (seconds first), e.g. \"0 0 3 * * *\"")
	}
	return t, nil
}
