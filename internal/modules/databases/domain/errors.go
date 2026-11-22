package domain

import (
	"errors"
)

// ErrBackupsDisabled : restauration impossible sans sauvegardes configurées.
var ErrBackupsDisabled = errors.New("backups are not configured on this platform")

// ErrBackupPluginMissing : sauvegardes configurées mais plugin Barman Cloud absent.
var ErrBackupPluginMissing = errors.New("the Barman Cloud plugin is not installed on the cluster")

// ErrOperatorMissing : CloudNativePG n'est pas installé.
var ErrOperatorMissing = errors.New("CloudNativePG is not installed on the cluster")

var ErrAppSecretMissing = errors.New("connection secret not created yet")
