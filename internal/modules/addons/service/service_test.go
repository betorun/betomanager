package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thekrauss/kubemanager/internal/modules/addons/domain"
	dauth "github.com/thekrauss/kubemanager/internal/modules/auth/domain"
	"github.com/thekrauss/kubemanager/internal/modules/utils"
)

type fakeRepo struct {
	rows      map[uuid.UUID]domain.Addon
	attached  []domain.Attachment
	createErr error
}

func (r *fakeRepo) Create(_ context.Context, a *domain.Addon) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.rows[a.ID] = *a
	return nil
}
func (r *fakeRepo) List(_ context.Context, p uuid.UUID) ([]domain.Addon, error) {
	var out []domain.Addon
	for _, a := range r.rows {
		if a.ProjectID == p {
			out = append(out, a)
		}
	}
	return out, nil
}
func (r *fakeRepo) Get(_ context.Context, p, id uuid.UUID) (*domain.Addon, error) {
	a, ok := r.rows[id]
	if !ok || a.ProjectID != p {
		return nil, domain.ErrNotFound
	}
	return &a, nil
}
func (r *fakeRepo) Count(ctx context.Context, p uuid.UUID) (int64, error) {
	l, _ := r.List(ctx, p)
	return int64(len(l)), nil
}
func (r *fakeRepo) SetVersion(_ context.Context, id uuid.UUID, v string) error {
	a := r.rows[id]
	a.Version = v
	r.rows[id] = a
	return nil
}
func (r *fakeRepo) Delete(_ context.Context, id uuid.UUID) error { delete(r.rows, id); return nil }
func (r *fakeRepo) SaveAttachment(_ context.Context, a *domain.Attachment) error {
	r.attached = append(r.attached, *a)
	return nil
}
func (r *fakeRepo) ListAttachments(_ context.Context, ids []uuid.UUID) ([]domain.Attachment, error) {
	var out []domain.Attachment
	for _, a := range r.attached {
		for _, id := range ids {
			if a.AddonID == id {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

type fakeWorkloads struct {
	project  uuid.UUID
	deployed []Carrier
	ids      map[string]uuid.UUID
	secrets  map[uuid.UUID]map[string]string
	deleted  []uuid.UUID
	images   map[uuid.UUID]string
	names    map[string]bool
	waits    map[uuid.UUID][]string
	running  bool // porteurs en marche
}

func (w *fakeWorkloads) Deploy(_ context.Context, c Carrier) (uuid.UUID, error) {
	id := uuid.New()
	w.deployed = append(w.deployed, c)
	w.ids[c.Name] = id
	w.secrets[id] = c.Secrets
	return id, nil
}
func (w *fakeWorkloads) Delete(_ context.Context, id uuid.UUID) error {
	w.deleted = append(w.deleted, id)
	return nil
}
func (w *fakeWorkloads) SetImage(_ context.Context, id uuid.UUID, image string) error {
	w.images[id] = image
	return nil
}
func (w *fakeWorkloads) States(context.Context, uuid.UUID) (map[uuid.UUID]CarrierState, error) {
	if w.running {
		out := map[uuid.UUID]CarrierState{}
		for _, id := range w.ids {
			out[id] = CarrierState{Status: utils.WorkloadRunning}
		}
		return out, nil
	}
	return map[uuid.UUID]CarrierState{}, nil
}
func (w *fakeWorkloads) NameTaken(_ context.Context, _ uuid.UUID, name string) (bool, error) {
	return w.names[name], nil
}
func (w *fakeWorkloads) ProjectOf(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	if id == uuid.Nil {
		return uuid.Nil, errors.New("not found")
	}
	return w.project, nil
}
func (w *fakeWorkloads) NameOf(context.Context, uuid.UUID) (string, error) { return "api", nil }
func (w *fakeWorkloads) Secret(_ context.Context, id uuid.UUID, name string) (string, error) {
	return w.secrets[id][name], nil
}
func (w *fakeWorkloads) WaitFor(_ context.Context, id uuid.UUID, target string) error {
	if w.waits == nil {
		w.waits = map[uuid.UUID][]string{}
	}
	w.waits[id] = append(w.waits[id], target)
	return nil
}
func (w *fakeWorkloads) SetSecrets(_ context.Context, id uuid.UUID, values map[string]string) (bool, error) {
	if w.secrets[id] == nil {
		w.secrets[id] = map[string]string{}
	}
	for k, v := range values {
		w.secrets[id][k] = v
	}
	return true, nil
}

type fakeProjects struct{ p dauth.Project }

func (f fakeProjects) GetProjectByID(context.Context, string) (*dauth.Project, error) {
	return &f.p, nil
}

func newTestService(t *testing.T, maxAddons int) (*Service, *fakeRepo, *fakeWorkloads, uuid.UUID) {
	t.Helper()
	project := uuid.New()
	repo := &fakeRepo{rows: map[uuid.UUID]domain.Addon{}}
	wl := &fakeWorkloads{project: project, ids: map[string]uuid.UUID{}, secrets: map[uuid.UUID]map[string]string{},
		images: map[uuid.UUID]string{}, names: map[string]bool{}}
	p := dauth.Project{Status: utils.ProjectStatusReady, MaxAddons: maxAddons}
	return &Service{Repo: repo, Projects: fakeProjects{p}, Workloads: wl}, repo, wl, project
}

func TestCreate_PersistentRedisDefaults(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "files-taches"}, "user:1")
	require.NoError(t, err)
	require.Len(t, wl.deployed, 1)
	c := wl.deployed[0]
	assert.Equal(t, "valkey/valkey:9.1.4", c.Image)
	assert.Equal(t, "addon/redis/"+v.ID, c.ManagedBy, "conteneur marqué comme géré par le service")
	assert.Equal(t, 6379, c.Port)
	assert.Equal(t, "1Gi", c.Disk, "persistant par défaut : disque")
	assert.Equal(t, "512Mi", c.Memory)
	cmd := strings.Join(c.Command, " ")
	assert.Contains(t, cmd, "--requirepass $(ADDON_PASSWORD)", "le mot de passe passe par la variable, pas en clair")
	assert.Contains(t, cmd, "--appendonly yes")
	assert.Contains(t, cmd, "--maxmemory-policy noeviction")
	assert.Contains(t, cmd, "--maxmemory 429496729", "80 % de 512 Mio")
	pwd := c.Secrets[domain.PasswordVariable]
	assert.Len(t, pwd, 64)
	assert.NotContains(t, cmd, pwd)
	assert.NotContains(t, strings.Join(c.Health, " "), pwd)

	assert.Equal(t, "files-taches:6379", v.Host)
	assert.Equal(t, "Redis (via Valkey)", v.Title)
	assert.Equal(t, "valkey", v.Logo)
	assert.Equal(t, "REDIS_URL", v.Variable)
	assert.False(t, v.UpgradeAvailable)
}

func TestCreate_CacheHasNoDisk(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	_, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache", Mode: "cache", Size: "mini"}, "")
	require.NoError(t, err)
	c := wl.deployed[0]
	assert.Empty(t, c.Disk)
	cmd := strings.Join(c.Command, " ")
	assert.Contains(t, cmd, "--maxmemory-policy allkeys-lru")
	assert.Contains(t, cmd, "--appendonly no")

	_, err = s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache2", Mode: "cache", Disk: "1Gi"}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidDisk, "cache : pas de disque")
}

func TestCreate_Validation(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	wl.names["api"] = true
	cases := map[string]struct {
		in   domain.CreateAddonRequest
		want error
	}{
		"modèle inconnu": {domain.CreateAddonRequest{Template: "mongodb", Name: "db"}, domain.ErrUnknownTemplate},
		"nom invalide":   {domain.CreateAddonRequest{Template: "redis", Name: "Cache!"}, domain.ErrInvalidName},
		"version":        {domain.CreateAddonRequest{Template: "redis", Name: "cache", Version: "6.0"}, domain.ErrInvalidVersion},
		"usage":          {domain.CreateAddonRequest{Template: "redis", Name: "cache", Mode: "cluster"}, domain.ErrInvalidMode},
		"taille":         {domain.CreateAddonRequest{Template: "redis", Name: "cache", Size: "huge"}, domain.ErrInvalidSize},
		"disque":         {domain.CreateAddonRequest{Template: "redis", Name: "cache", Disk: "50Gi"}, domain.ErrInvalidDisk},
		"nom pris":       {domain.CreateAddonRequest{Template: "redis", Name: "api"}, domain.ErrNameTaken},
	}
	for name, c := range cases {
		_, err := s.Create(context.Background(), project, c.in, "")
		assert.ErrorIs(t, err, c.want, name)
	}
	assert.Empty(t, wl.deployed, "aucun conteneur créé sur une demande refusée")
}

func TestCreate_LimitPerProject(t *testing.T) {
	s, _, _, project := newTestService(t, 2)
	for _, n := range []string{"cache-a", "cache-b"} {
		_, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: n}, "")
		require.NoError(t, err)
	}
	_, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache-c"}, "")
	assert.ErrorIs(t, err, domain.ErrLimitReached)
}

func TestCreate_RegistrationFailureRemovesCarrier(t *testing.T) {
	s, repo, wl, project := newTestService(t, 0)
	repo.createErr = domain.ErrNameTaken
	_, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache"}, "")
	assert.ErrorIs(t, err, domain.ErrNameTaken)
	require.Len(t, wl.deleted, 1)
	assert.Equal(t, wl.ids["cache"], wl.deleted[0])
}

func TestAttach_SetsURLAsSecret(t *testing.T) {
	s, repo, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache"}, "")
	require.NoError(t, err)
	id := uuid.MustParse(v.ID)
	api := uuid.New()

	res, err := s.Attach(context.Background(), project, id, api, "")
	require.NoError(t, err)
	assert.Equal(t, "REDIS_URL", res.Variable)
	pwd := wl.secrets[wl.ids["cache"]][domain.PasswordVariable]
	assert.Equal(t, "redis://default:"+pwd+"@cache:6379", wl.secrets[api]["REDIS_URL"])
	assert.Equal(t, []string{"cache:6379"}, wl.waits[api], "le conteneur attend le service au démarrage")
	require.Len(t, repo.attached, 1)

	_, err = s.Attach(context.Background(), project, id, wl.ids["cache"], "")
	assert.ErrorIs(t, err, domain.ErrSelfAttach)
	_, err = s.Attach(context.Background(), project, id, api, "redis-url")
	assert.ErrorIs(t, err, domain.ErrInvalidVariable)
	wl.project = uuid.New() // conteneur d'un autre projet
	_, err = s.Attach(context.Background(), project, id, api, "")
	assert.ErrorIs(t, err, domain.ErrWorkloadNotFound)
}

func TestUpgrade_NeverGoesBack(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache", Version: "8.1"}, "")
	require.NoError(t, err)
	assert.True(t, v.UpgradeAvailable)
	id := uuid.MustParse(v.ID)

	_, err = s.Upgrade(context.Background(), project, id, "9.0")
	require.NoError(t, err)
	assert.Equal(t, "valkey/valkey:9.0.8", wl.images[wl.ids["cache"]])

	_, err = s.Upgrade(context.Background(), project, id, "8.1")
	assert.ErrorIs(t, err, domain.ErrNoUpgrade, "retour en arrière refusé")
	_, err = s.Upgrade(context.Background(), project, id, "9.0")
	assert.ErrorIs(t, err, domain.ErrNoUpgrade, "même version")

	up, err := s.Upgrade(context.Background(), project, id, "")
	require.NoError(t, err)
	assert.Equal(t, "9.1", up.Version)
	assert.False(t, up.UpgradeAvailable)
}

func TestView_MajorUpgradeFlag(t *testing.T) {
	s, _, _, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache", Version: "8.1"}, "")
	require.NoError(t, err)
	assert.True(t, v.MajorUpgrade, "8.1 → 9.1 : version majeure")
	v, err = s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache2", Version: "9.0"}, "")
	require.NoError(t, err)
	assert.True(t, v.UpgradeAvailable)
	assert.False(t, v.MajorUpgrade)
}

func TestDelete_ReportsStillAttached(t *testing.T) {
	s, repo, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "redis", Name: "cache"}, "")
	require.NoError(t, err)
	id := uuid.MustParse(v.ID)
	_, err = s.Attach(context.Background(), project, id, uuid.New(), "")
	require.NoError(t, err)

	res, err := s.Delete(context.Background(), project, id)
	require.NoError(t, err)
	assert.Len(t, res.StillAttached, 1)
	assert.Equal(t, []uuid.UUID{wl.ids["cache"]}, wl.deleted)
	assert.Empty(t, repo.rows)
}

func TestCreateAndAttach_MariaDB(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "mariadb", Name: "shop-db"}, "")
	require.NoError(t, err)
	c := wl.deployed[0]
	assert.Equal(t, "mariadb:11.8.9", c.Image)
	assert.Equal(t, "1Gi", c.Disk, "persistant par défaut, premier disque")
	assert.Equal(t, "/var/lib/mysql", c.DiskPath)
	assert.Equal(t, int64(999), c.RunAsUser)
	pwd := c.Secrets[domain.PasswordVariable]
	require.Len(t, pwd, 64)
	assert.Equal(t, pwd, c.Secrets["MARIADB_PASSWORD"])
	assert.Equal(t, pwd, c.Secrets["MARIADB_ROOT_PASSWORD"])
	assert.Equal(t, "app", c.Env["MARIADB_USER"])
	assert.Equal(t, "localhost", c.Env["MARIADB_ROOT_HOST"], "root seulement par le socket local")
	assert.Contains(t, c.Command, "--innodb-buffer-pool-size=268435456", "moitié des 512 Mi de la taille small")
	for _, part := range append(append([]string{}, c.Command...), c.Health...) {
		assert.NotContains(t, part, pwd, "jamais de mot de passe en argument")
	}
	for k, val := range c.Env {
		assert.NotContains(t, val, pwd, "mot de passe dans une variable non secrète : %s", k)
	}
	_, err = s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "mariadb", Name: "cache-db", Mode: "cache"}, "")
	assert.ErrorIs(t, err, domain.ErrInvalidMode, "MariaDB : persistant seulement")

	api := uuid.New()
	res, err := s.Attach(context.Background(), project, uuid.MustParse(v.ID), api, "")
	require.NoError(t, err)
	assert.Equal(t, "DATABASE_URL", res.Variable)
	assert.Empty(t, res.KeyVariable)
	assert.Equal(t, "mysql://app:"+pwd+"@shop-db:3306/app", wl.secrets[api]["DATABASE_URL"])
}

func TestCreateAndAttach_MeilisearchKeyInSecondVariable(t *testing.T) {
	s, _, wl, project := newTestService(t, 0)
	v, err := s.Create(context.Background(), project, domain.CreateAddonRequest{Template: "meilisearch", Name: "search", Size: "medium"}, "")
	require.NoError(t, err)
	c := wl.deployed[0]
	assert.Empty(t, c.Command, "commande de l'image")
	pwd := c.Secrets[domain.PasswordVariable]
	assert.Equal(t, pwd, c.Secrets["MEILI_MASTER_KEY"])
	assert.Equal(t, "production", c.Env["MEILI_ENV"])
	assert.Equal(t, "644245094", c.Env["MEILI_MAX_INDEXING_MEMORY"], "60 % de 1 Gi")

	api := uuid.New()
	res, err := s.Attach(context.Background(), project, uuid.MustParse(v.ID), api, "")
	require.NoError(t, err)
	assert.Equal(t, "MEILISEARCH_URL", res.Variable)
	assert.Equal(t, "MEILISEARCH_KEY", res.KeyVariable)
	assert.Equal(t, "http://search:7700", wl.secrets[api]["MEILISEARCH_URL"], "adresse sans identifiants")
	assert.Equal(t, pwd, wl.secrets[api]["MEILISEARCH_KEY"])

	other := uuid.New()
	res, err = s.Attach(context.Background(), project, uuid.MustParse(v.ID), other, "SEARCH")
	require.NoError(t, err)
	assert.Equal(t, "SEARCH_KEY", res.KeyVariable)
	assert.Equal(t, pwd, wl.secrets[other]["SEARCH_KEY"])
}

func TestCatalog_ImagesArePinned(t *testing.T) {
	for _, tpl := range domain.Catalog {
		require.NotEmpty(t, tpl.Versions, tpl.ID)
		assert.Equal(t, tpl.Recommended, tpl.Versions[0].ID, "%s : la recommandée d'abord", tpl.ID)
		for _, ver := range tpl.Versions {
			assert.Regexp(t, `:v?\d+\.\d+\.\d+$`, ver.Image, "%s : image figée", tpl.ID)
		}
		_, announced := domain.UpcomingByID(tpl.ID)
		assert.False(t, announced, "%s : ouvert et encore annoncé", tpl.ID)
	}
}
