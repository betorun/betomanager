package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	"github.com/thekrauss/kubemanager/internal/modules/addons/service"
)

func TestBackupJob_SecretsStayOutOfArguments(t *testing.T) {
	tpl := domain.MariaDB
	job := backupJob(service.BackupTask{TaskID: "t1", Namespace: "km-x", Host: "shop-db", Release: "shop-db", Image: "mariadb:11.8.9",
		RunAsUser: 999, Commands: *tpl.Backup, URL: "https://api/x?sig=s", FetchImage: "curl"}, "p1", false)
	pod := job.Spec.Template.Spec
	assert.Equal(t, "p1", job.Spec.Template.Labels["km-project-id"], "accepté par le réseau du projet")
	require.Len(t, pod.InitContainers, 1)
	dump := pod.InitContainers[0]
	assert.Equal(t, "mariadb:11.8.9", dump.Image)
	var pwd bool
	for _, e := range dump.Env {
		if e.Name == "MYSQL_PWD" {
			pwd = e.ValueFrom != nil && e.ValueFrom.SecretKeyRef.Name == "shop-db-env" && e.ValueFrom.SecretKeyRef.Key == "ADDON_PASSWORD"
		}
	}
	assert.True(t, pwd, "mot de passe lu dans le secret du service")
	assert.Equal(t, "upload", pod.Containers[0].Name)
	assert.Equal(t, int64(999), *pod.SecurityContext.RunAsUser)

	restore := backupJob(service.BackupTask{TaskID: "t2", Release: "shop-db", Commands: *tpl.Backup, Image: "mariadb:11.8.9", FetchImage: "curl"}, "p1", true)
	assert.Equal(t, "fetch", restore.Spec.Template.Spec.InitContainers[0].Name)
	assert.Contains(t, restore.Spec.Template.Spec.Containers[0].Command[2], "gunzip")
}
