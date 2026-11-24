package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

type memRepo struct {
	mu      sync.Mutex
	buckets map[uuid.UUID]domain.Bucket
	keys    map[uuid.UUID]domain.Key
}

func newRepo() *memRepo {
	return &memRepo{buckets: map[uuid.UUID]domain.Bucket{}, keys: map[uuid.UUID]domain.Key{}}
}

func (m *memRepo) CreateBucket(_ context.Context, b *domain.Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[b.ID] = *b
	return nil
}
func (m *memRepo) ListBuckets(_ context.Context, p uuid.UUID) ([]domain.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Bucket
	for _, b := range m.buckets {
		if b.ProjectID == p {
			out = append(out, b)
		}
	}
	return out, nil
}
func (m *memRepo) GetBucket(_ context.Context, p, id uuid.UUID) (*domain.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[id]
	if !ok || b.ProjectID != p {
		return nil, domain.ErrNotFound
	}
	return &b, nil
}
func (m *memRepo) UpdateBucket(_ context.Context, b *domain.Bucket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[b.ID] = *b
	return nil
}
func (m *memRepo) DeleteBucket(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.buckets, id)
	return nil
}
func (m *memRepo) AllBuckets(context.Context) ([]domain.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Bucket
	for _, b := range m.buckets {
		out = append(out, b)
	}
	return out, nil
}
func (m *memRepo) CreateKey(_ context.Context, k *domain.Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = *k
	return nil
}
func (m *memRepo) ListKeys(_ context.Context, b uuid.UUID) ([]domain.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Key
	for _, k := range m.keys {
		if k.BucketID == b {
			out = append(out, k)
		}
	}
	return out, nil
}
func (m *memRepo) GetKey(_ context.Context, b, id uuid.UUID) (*domain.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.BucketID != b {
		return nil, domain.ErrKeyNotFound
	}
	return &k, nil
}
func (m *memRepo) UpdateKey(_ context.Context, k *domain.Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = *k
	return nil
}
func (m *memRepo) ProjectBuckets(ctx context.Context, p uuid.UUID) ([]domain.Bucket, error) {
	return m.ListBuckets(ctx, p)
}
func (m *memRepo) DeleteKey(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keys, id)
	return nil
}

// fakeBackend : buckets (octets occupés) et identités (bucket, lecture seule).
type fakeBackend struct {
	buckets    map[string]int64
	identities map[string]identity
	purged     []string
	n          int
	public     map[string]bool
	objects    map[string]map[string]int64 // bucket → nom → taille
}

type identity struct {
	bucket string
	access string
	keys   []string
}

// readOnly : l'identité ne peut plus écrire.
func (i identity) readOnly() bool { return i.access != domain.AccessReadWrite }

func newBackend() *fakeBackend {
	return &fakeBackend{buckets: map[string]int64{}, identities: map[string]identity{},
		public: map[string]bool{}, objects: map[string]map[string]int64{}}
}

func (f *fakeBackend) Name() string { return "test" }
func (f *fakeBackend) CreateBucket(_ context.Context, b string) error {
	f.buckets[b] = 0
	return nil
}
func (f *fakeBackend) DeleteBucket(_ context.Context, b string, purge bool) error {
	if !purge && f.buckets[b] > 0 {
		return domain.ErrNotEmpty
	}
	if purge {
		f.purged = append(f.purged, b)
	}
	delete(f.buckets, b)
	return nil
}
func (f *fakeBackend) Usage(_ context.Context, b string) (int64, error) { return f.buckets[b], nil }
func (f *fakeBackend) CreateIdentity(_ context.Context, id, b, access string) (Credentials, error) {
	f.n++
	ak := fmt.Sprintf("AK%d", f.n)
	f.identities[id] = identity{bucket: b, access: access, keys: []string{ak}}
	return Credentials{AccessKeyID: ak, SecretAccessKey: "secret-" + ak}, nil
}
func (f *fakeBackend) SetIdentityAccess(_ context.Context, id, b, access string) error {
	i := f.identities[id]
	i.bucket, i.access = b, access
	f.identities[id] = i
	return nil
}
func (f *fakeBackend) AddAccessKey(_ context.Context, id string) (Credentials, error) {
	f.n++
	ak := fmt.Sprintf("AK%d", f.n)
	i := f.identities[id]
	i.keys = append(i.keys, ak)
	f.identities[id] = i
	return Credentials{AccessKeyID: ak, SecretAccessKey: "secret-" + ak}, nil
}
func (f *fakeBackend) DeleteAccessKey(_ context.Context, id, ak string) error {
	i := f.identities[id]
	i.keys = slices.DeleteFunc(i.keys, func(k string) bool { return k == ak })
	f.identities[id] = i
	return nil
}
func (f *fakeBackend) SetPublic(_ context.Context, b string, public bool) error {
	f.public[b] = public
	return nil
}
func (f *fakeBackend) ListObjects(_ context.Context, b, prefix, _ string, _ int) (domain.ObjectPage, error) {
	page := domain.ObjectPage{Prefix: prefix}
	for k, n := range f.objects[b] {
		if strings.HasPrefix(k, prefix) {
			page.Objects = append(page.Objects, domain.Object{Key: k, Size: n})
		}
	}
	return page, nil
}
func (f *fakeBackend) GetObject(_ context.Context, b, k string) (io.ReadCloser, domain.Object, error) {
	n, ok := f.objects[b][k]
	if !ok {
		return nil, domain.Object{}, domain.ErrObjectNotFound
	}
	return io.NopCloser(strings.NewReader(strings.Repeat("x", int(n)))), domain.Object{Key: k, Size: n}, nil
}
func (f *fakeBackend) PutObject(_ context.Context, b, k string, r io.Reader, size int64, _ string) error {
	if f.objects[b] == nil {
		f.objects[b] = map[string]int64{}
	}
	f.objects[b][k] = size
	f.buckets[b] += size
	return nil
}
func (f *fakeBackend) DeleteObjects(_ context.Context, b string, keys []string) (int, error) {
	n := 0
	for _, k := range keys {
		for name, size := range f.objects[b] {
			if name == k || (strings.HasSuffix(k, "/") && strings.HasPrefix(name, k)) {
				delete(f.objects[b], name)
				f.buckets[b] -= size
				n++
			}
		}
	}
	return n, nil
}
func (f *fakeBackend) PresignGet(_ context.Context, b, k string, ttl time.Duration) (string, error) {
	return fmt.Sprintf("https://s3.example/%s/%s?ttl=%s", b, k, ttl), nil
}
func (f *fakeBackend) DeleteIdentity(_ context.Context, id string) error {
	delete(f.identities, id)
	return nil
}

type readyProjects struct{}

func (readyProjects) ProjectReady(context.Context, uuid.UUID) error { return nil }

type fakeWorkloads struct {
	project uuid.UUID
	env     map[string]string
	secrets map[string]string
	fail    error
}

func (w *fakeWorkloads) ProjectOf(context.Context, uuid.UUID) (uuid.UUID, error) {
	return w.project, nil
}
func (w *fakeWorkloads) NameOf(context.Context, uuid.UUID) (string, error) { return "web", nil }
func (w *fakeWorkloads) SetVariables(_ context.Context, _ uuid.UUID, env, secrets map[string]string, unset []string) (bool, error) {
	if w.fail != nil {
		return false, w.fail
	}
	for k, v := range env {
		w.env[k] = v
	}
	for k, v := range secrets {
		w.secrets[k] = v
	}
	for _, k := range unset {
		delete(w.env, k)
		delete(w.secrets, k)
	}
	return true, nil
}

func newService(p uuid.UUID) (*Service, *memRepo, *fakeBackend, *fakeWorkloads) {
	repo, be := newRepo(), newBackend()
	w := &fakeWorkloads{project: p, env: map[string]string{}, secrets: map[string]string{}}
	return &Service{Repo: repo, Backend: be, Projects: readyProjects{}, Workloads: w,
		Endpoint: "https://s3.example", Region: "us-east-1", Logger: zap.NewNop().Sugar()}, repo, be, w
}

func TestCreateValidatesAndNamesBuckets(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	if _, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "Uploads"}, "u"); !errors.Is(err, domain.ErrInvalidName) {
		t.Fatalf("nom invalide : %v", err)
	}
	if _, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads", QuotaGB: 3}, "u"); !errors.Is(err, domain.ErrInvalidQuota) {
		t.Fatalf("quota invalide : %v", err)
	}
	v, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.Bucket, domain.BucketPrefix) || len(v.Bucket) != len(domain.BucketPrefix)+12 || v.QuotaBytes != 5*domain.GB {
		t.Fatalf("bucket : %+v", v)
	}
	if _, ok := be.buckets[v.Bucket]; !ok {
		t.Fatal("bucket S3 non créé")
	}
	if _, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u"); !errors.Is(err, domain.ErrNameTaken) {
		t.Fatalf("doublon : %v", err)
	}
	// Même nom dans un autre projet : autre nom S3.
	other := uuid.New()
	v2, err := s.Create(ctx, other, domain.CreateBucketRequest{Name: "uploads"}, "u")
	if err != nil || v2.Bucket == v.Bucket {
		t.Fatalf("autre projet : %v %s", err, v2.Bucket)
	}
	for i := 0; i < domain.MaxBucketsPerProject-1; i++ {
		if _, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "b" + strings.Repeat("x", i+2)}, "u"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(ctx, p, domain.CreateBucketRequest{Name: "one-more"}, "u"); !errors.Is(err, domain.ErrTooMany) {
		t.Fatalf("limite : %v", err)
	}
}

func TestAttachAndRevoke(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo, be, w := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	wl := uuid.New()
	if _, err := s.Attach(ctx, p, id, wl, "s3", "", "u"); !errors.Is(err, domain.ErrInvalidPrefix) {
		t.Fatalf("préfixe : %v", err)
	}
	out, err := s.Attach(ctx, p, id, wl, "", "", "u")
	if err != nil || !out.Redeploy || len(out.Variables) != 5 {
		t.Fatalf("branchement : %v %+v", err, out)
	}
	if w.env["S3_BUCKET"] != v.Bucket || w.env["S3_ENDPOINT"] != "https://s3.example" || w.secrets["S3_SECRET_ACCESS_KEY"] == "" {
		t.Fatalf("variables : env=%v", w.env)
	}
	if _, ok := w.env["S3_SECRET_ACCESS_KEY"]; ok {
		t.Fatal("secret posé en variable visible")
	}
	idn := domain.IdentityName(uuid.MustParse(out.Key.ID))
	if got := be.identities[idn]; got.bucket != v.Bucket || got.readOnly() {
		t.Fatalf("identité : %+v", got)
	}
	if _, err := s.Attach(ctx, p, id, wl, "", "", "u"); !errors.Is(err, domain.ErrAlreadyAttached) {
		t.Fatalf("second branchement : %v", err)
	}
	// Conteneur d'un autre projet : refusé.
	w.project = uuid.New()
	if _, err := s.Attach(ctx, p, id, uuid.New(), "", "", "u"); !errors.Is(err, domain.ErrWorkloadNotFound) {
		t.Fatalf("autre projet : %v", err)
	}
	w.project = p
	// Échec du redéploiement : ni clé ni identité orpheline.
	w.fail = errors.New("deploy in progress")
	before := len(be.identities)
	if _, err := s.Attach(ctx, p, id, uuid.New(), "", "", "u"); err == nil {
		t.Fatal("échec attendu")
	}
	if len(be.identities) != before || len(repo.keys) != 1 {
		t.Fatalf("restes : %d identités, %d clés", len(be.identities), len(repo.keys))
	}
	w.fail = nil
	// Révocation : variables retirées, identité supprimée.
	if err := s.DeleteKey(ctx, p, id, uuid.MustParse(out.Key.ID)); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.env["S3_BUCKET"]; ok || len(w.secrets) != 0 {
		t.Fatalf("variables restantes : %v %v", w.env, w.secrets)
	}
	if _, ok := be.identities[idn]; ok {
		t.Fatal("identité non supprimée")
	}
}

func TestManualKeyShowsSecretOnce(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	k, err := s.CreateKey(ctx, p, id, "ci", domain.AccessReadOnly, "u")
	if err != nil || k.SecretAccessKey == "" || k.Bucket != v.Bucket {
		t.Fatalf("clé : %v %+v", err, k)
	}
	if !be.identities[domain.IdentityName(uuid.MustParse(k.Key.ID))].readOnly() {
		t.Fatal("clé en lecture seule créée en écriture")
	}
	got, _ := s.Get(ctx, p, id)
	if len(got.Keys) != 1 || got.Keys[0].AccessKeyID != k.Key.AccessKeyID {
		t.Fatalf("liste : %+v", got.Keys)
	}
	if _, err := s.CreateKey(ctx, p, id, "CI", "", "u"); !errors.Is(err, domain.ErrInvalidKeyName) {
		t.Fatalf("nom : %v", err)
	}
}

func TestQuotaMakesBucketReadOnly(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads", QuotaGB: 1}, "u")
	id := uuid.MustParse(v.ID)
	rw, _ := s.CreateKey(ctx, p, id, "rw", domain.AccessReadWrite, "u")
	ro, _ := s.CreateKey(ctx, p, id, "ro", domain.AccessReadOnly, "u")
	rwID, roID := domain.IdentityName(uuid.MustParse(rw.Key.ID)), domain.IdentityName(uuid.MustParse(ro.Key.ID))

	be.buckets[v.Bucket] = domain.GB + 1
	if err := s.RefreshUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if b := repo.buckets[id]; !b.OverQuota || b.UsedBytes != domain.GB+1 || b.UsageAt == nil {
		t.Fatalf("relevé : %+v", b)
	}
	if !be.identities[rwID].readOnly() {
		t.Fatal("clé en écriture non bridée")
	}
	// Une nouvelle clé naît en lecture seule tant que le quota est dépassé.
	n, _ := s.CreateKey(ctx, p, id, "new", domain.AccessReadWrite, "u")
	if !be.identities[domain.IdentityName(uuid.MustParse(n.Key.ID))].readOnly() {
		t.Fatal("nouvelle clé en écriture au-delà du quota")
	}
	// Quota relevé : accès d'origine rendu, la clé en lecture seule le reste.
	if _, err := s.SetQuota(ctx, p, id, 5); err != nil {
		t.Fatal(err)
	}
	if repo.buckets[id].OverQuota || be.identities[rwID].readOnly() || !be.identities[roID].readOnly() {
		t.Fatalf("après quota : %+v %+v", be.identities[rwID], be.identities[roID])
	}
}

func TestDeleteNeedsConfirmWhenNotEmpty(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo, be, w := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	if _, err := s.Attach(ctx, p, id, uuid.New(), "", "", "u"); err != nil {
		t.Fatal(err)
	}
	be.buckets[v.Bucket] = 42
	if err := s.Delete(ctx, p, id, ""); !errors.Is(err, domain.ErrNotEmpty) {
		t.Fatalf("non vide : %v", err)
	}
	if err := s.Delete(ctx, p, id, "wrong"); !errors.Is(err, domain.ErrNotEmpty) {
		t.Fatalf("mauvaise confirmation : %v", err)
	}
	if err := s.Delete(ctx, p, id, "uploads"); err != nil {
		t.Fatal(err)
	}
	if len(be.purged) != 1 || len(be.identities) != 0 || len(repo.keys) != 0 || len(repo.buckets) != 0 || len(w.secrets) != 0 {
		t.Fatalf("restes : purged=%v ids=%d keys=%d buckets=%d secrets=%v", be.purged, len(be.identities), len(repo.keys), len(repo.buckets), w.secrets)
	}
	if _, err := s.Get(ctx, uuid.New(), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("autre projet : %v", err)
	}
}

func TestUnavailableWithoutBackend(t *testing.T) {
	s := &Service{Repo: newRepo()}
	if _, err := s.List(context.Background(), uuid.New()); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("sans S3 : %v", err)
	}
}
