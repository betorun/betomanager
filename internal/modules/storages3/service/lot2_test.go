package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

type alertCall struct {
	level int
	name  string
}

type fakeAlerts struct{ calls []alertCall }

func (a *fakeAlerts) BucketQuota(_ context.Context, _, _ uuid.UUID, level int, name string, _, _ int64) {
	a.calls = append(a.calls, alertCall{level, name})
}

func TestRotateAttachmentKeepsContainerWorking(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo, be, w := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	att, err := s.Attach(ctx, p, id, uuid.New(), "", "", "u")
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.MustParse(att.Key.ID)
	old := w.secrets["S3_ACCESS_KEY_ID"]

	got, err := s.RotateKey(ctx, p, id, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretAccessKey != "" || !got.Redeploy {
		t.Fatalf("branchement : le secret ne doit pas sortir, le conteneur doit être redéployé : %+v", got)
	}
	nw := w.secrets["S3_ACCESS_KEY_ID"]
	if nw == old || w.secrets["S3_SECRET_ACCESS_KEY"] != "secret-"+nw {
		t.Fatalf("variables non mises à jour : %v", w.secrets)
	}
	keys := be.identities[domain.IdentityName(keyID)].keys
	if len(keys) != 1 || keys[0] != nw {
		t.Fatalf("ancienne clé non supprimée : %v", keys)
	}
	if k := repo.keys[keyID]; k.AccessKeyID != nw || k.RotatedAt == nil {
		t.Fatalf("ligne non mise à jour : %+v", k)
	}
}

func TestRotateManualKeyShowsNewSecretOnce(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	k, _ := s.CreateKey(ctx, p, id, "ci", "", "u")
	got, err := s.RotateKey(ctx, p, id, uuid.MustParse(k.Key.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretAccessKey == "" || got.Key.AccessKeyID == k.Key.AccessKeyID || got.Bucket != v.Bucket {
		t.Fatalf("rotation : %+v", got)
	}
	if keys := be.identities[domain.IdentityName(uuid.MustParse(k.Key.ID))].keys; len(keys) != 1 {
		t.Fatalf("clés de l'identité : %v", keys)
	}
}

func TestOverQuotaKeepsDeleteButRefusesUpload(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	al := &fakeAlerts{}
	s.Alerts = al
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads", QuotaGB: 1}, "u")
	id := uuid.MustParse(v.ID)
	rw, _ := s.CreateKey(ctx, p, id, "rw", "", "u")

	// Plus de 80 % : alerte à 80, accès intact.
	used := domain.GB - 10<<20
	be.objects[v.Bucket] = map[string]int64{"a/big.bin": used}
	be.buckets[v.Bucket] = used
	if err := s.RefreshUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if len(al.calls) != 1 || al.calls[0].level != 80 || al.calls[0].name != "uploads" {
		t.Fatalf("alertes : %+v", al.calls)
	}
	// Un envoi qui ferait dépasser est refusé.
	if _, err := s.PutObject(ctx, p, id, "b.bin", strings.NewReader("x"), 20<<20, ""); !errors.Is(err, domain.ErrOverQuota) {
		t.Fatalf("envoi au-delà du quota : %v", err)
	}
	// Au-delà du quota : lire et supprimer, plus écrire ; alerte à 100.
	be.objects[v.Bucket]["a/more.bin"] = domain.GB / 5
	be.buckets[v.Bucket] += domain.GB / 5
	_ = s.RefreshUsage(ctx)
	if got := be.identities[domain.IdentityName(uuid.MustParse(rw.Key.ID))].access; got != domain.AccessReadDelete {
		t.Fatalf("accès au-delà du quota : %s", got)
	}
	if last := al.calls[len(al.calls)-1]; last.level != 100 {
		t.Fatalf("alerte : %+v", last)
	}
	// Vider un dossier rend l'écriture aussitôt.
	res, err := s.DeleteObjects(ctx, p, id, []string{"a/"})
	if err != nil || res.Deleted != 2 {
		t.Fatalf("suppression : %+v %v", res, err)
	}
	if got := be.identities[domain.IdentityName(uuid.MustParse(rw.Key.ID))].access; got != domain.AccessReadWrite {
		t.Fatalf("accès après suppression : %s", got)
	}
	if _, err := s.PutObject(ctx, p, id, "b.bin", strings.NewReader("xyz"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
}

func TestObjectNamesAreValidated(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, _, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	for _, k := range []string{"", "/abs", "a/../b", "a\x00b"} {
		if _, err := s.PutObject(ctx, p, id, k, strings.NewReader(""), 0, ""); !errors.Is(err, domain.ErrInvalidObject) {
			t.Errorf("%q : %v", k, err)
		}
	}
	if _, err := s.PutObject(ctx, p, id, "dir/", strings.NewReader("x"), 1, ""); !errors.Is(err, domain.ErrInvalidObject) {
		t.Errorf("dossier non vide : %v", err)
	}
	if o, err := s.PutObject(ctx, p, id, "dir/", strings.NewReader(""), 0, ""); err != nil || !o.Dir {
		t.Errorf("nouveau dossier : %+v %v", o, err)
	}
	if _, err := s.ListObjects(ctx, p, id, "nodir", "", 0); !errors.Is(err, domain.ErrInvalidObject) {
		t.Errorf("préfixe sans / : %v", err)
	}
	if _, err := s.PutObject(ctx, p, id, "x", strings.NewReader(""), domain.MaxConsoleUpload+1, ""); !errors.Is(err, domain.ErrObjectTooLarge) {
		t.Errorf("trop gros : %v", err)
	}
}

func TestShareLink(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "uploads"}, "u")
	id := uuid.MustParse(v.ID)
	if _, err := s.ShareLink(ctx, p, id, "missing.txt", 0); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Fatalf("fichier absent : %v", err)
	}
	be.objects[v.Bucket] = map[string]int64{"doc.pdf": 10}
	for _, h := range []int{-1, 169} {
		if _, err := s.ShareLink(ctx, p, id, "doc.pdf", h); !errors.Is(err, domain.ErrInvalidExpiry) {
			t.Errorf("%d h : %v", h, err)
		}
	}
	l, err := s.ShareLink(ctx, p, id, "doc.pdf", 0)
	if err != nil || !strings.Contains(l.URL, "ttl=24h0m0s") {
		t.Fatalf("lien : %+v %v", l, err)
	}
}

func TestPublicAccess(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, _, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "assets"}, "u")
	id := uuid.MustParse(v.ID)
	got, err := s.SetPublic(ctx, p, id, true)
	if err != nil || !got.Public || !be.public[v.Bucket] || got.PublicURL != "https://s3.example/"+v.Bucket+"/" {
		t.Fatalf("public : %+v %v", got, err)
	}
	got, _ = s.SetPublic(ctx, p, id, false)
	if got.Public || be.public[v.Bucket] || got.PublicURL != "" {
		t.Fatalf("privé : %+v", got)
	}
}

func TestGracePeriodSuspendsThenPurges(t *testing.T) {
	ctx, p := context.Background(), uuid.New()
	s, repo, be, _ := newService(p)
	v, _ := s.Create(ctx, p, domain.CreateBucketRequest{Name: "assets"}, "u")
	id := uuid.MustParse(v.ID)
	ro, _ := s.CreateKey(ctx, p, id, "ro", domain.AccessReadOnly, "u")
	_, _ = s.SetPublic(ctx, p, id, true)
	roID := domain.IdentityName(uuid.MustParse(ro.Key.ID))

	if err := s.SuspendProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if be.identities[roID].access != domain.AccessNone || be.public[v.Bucket] || !repo.buckets[id].Suspended || !repo.buckets[id].Public {
		t.Fatalf("suspendu : %+v public=%v %+v", be.identities[roID], be.public[v.Bucket], repo.buckets[id])
	}
	if _, err := s.CreateKey(ctx, p, id, "xx", "", "u"); !errors.Is(err, domain.ErrSuspended) {
		t.Fatalf("clé pendant la grâce : %v", err)
	}
	if err := s.ResumeProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if be.identities[roID].access != domain.AccessReadOnly || !be.public[v.Bucket] || repo.buckets[id].Suspended {
		t.Fatal("restauration incomplète")
	}
	if err := s.PurgeProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(repo.buckets) != 0 || len(repo.keys) != 0 || len(be.identities) != 0 || len(be.purged) != 1 {
		t.Fatalf("purge : %d buckets, %d clés, %d identités", len(repo.buckets), len(repo.keys), len(be.identities))
	}
}
