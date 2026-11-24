package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// Lot 2 sur un vrai S3 : accès selon l'état (quota dépassé, grâce),
// rotation, lecture publique, objets et liens signés.
func TestS3_E2E_Lot2(t *testing.T) {
	if os.Getenv("STORAGE_S3_E2E") != "1" {
		t.Skip("STORAGE_S3_E2E=1 pour l'essai sur un vrai S3")
	}
	ctx := context.Background()
	endpoint := env("STORAGE_S3_ENDPOINT", "127.0.0.1:8333")
	s, err := NewS3(S3Config{Endpoint: endpoint, PublicEndpoint: "http://" + endpoint,
		AccessKey: env("STORAGE_S3_ACCESS_KEY", "betorun-storage-dev"), SecretKey: env("STORAGE_S3_SECRET_KEY", "storage-dev-secret")})
	if err != nil {
		t.Fatal(err)
	}
	a := domain.NewBucketName()
	if err := s.CreateBucket(ctx, a); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DeleteBucket(context.Background(), a, true) }()

	// ── Objets par l'identité du module ──
	for _, k := range []string{"img/a.png", "img/b.png", "img/sub/c.png", "readme.txt"} {
		if err := s.PutObject(ctx, a, k, strings.NewReader("data-"+k), int64(len("data-"+k)), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	root, err := s.ListObjects(ctx, a, "", "", 100)
	if err != nil || len(root.Objects) != 2 || !root.Objects[0].Dir || root.Objects[0].Key != "img/" || root.Objects[1].Key != "readme.txt" {
		t.Fatalf("racine : %+v %v", root, err)
	}
	img, _ := s.ListObjects(ctx, a, "img/", "", 100)
	if len(img.Objects) != 3 || img.Objects[0].Key != "img/sub/" {
		t.Fatalf("img/ : %+v", img)
	}
	page, _ := s.ListObjects(ctx, a, "img/", "", 1)
	if len(page.Objects) != 1 || page.Next == "" {
		t.Fatalf("pagination : %+v", page)
	}
	rc, info, err := s.GetObject(ctx, a, "readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "data-readme.txt" || info.Size != int64(len(body)) {
		t.Fatalf("lecture : %q %+v", body, info)
	}
	if _, _, err := s.GetObject(ctx, a, "absent.txt"); err != domain.ErrObjectNotFound {
		t.Fatalf("absent : %v", err)
	}

	// ── Lien signé à l'adresse publique, sans compte ──
	link, err := s.PresignGet(ctx, a, "readme.txt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpGet(t, link); got != http.StatusOK {
		t.Fatalf("lien signé : %d", got)
	}
	plain := "http://" + endpoint + "/" + a + "/readme.txt"
	if got := httpGet(t, plain); got == http.StatusOK {
		t.Fatal("privé : lecture anonyme acceptée")
	}

	// ── Lecture publique : lire un objet oui, lister non ──
	if err := s.SetPublic(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if httpGet(t, plain) != http.StatusOK {
			return errStill
		}
		return nil
	})
	if got := httpGet(t, "http://"+endpoint+"/"+a+"/"); got == http.StatusOK {
		t.Fatal("public : liste anonyme acceptée")
	}
	if err := s.SetPublic(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if httpGet(t, plain) == http.StatusOK {
			return errStill
		}
		return nil
	})

	// ── Accès d'une clé selon l'état du bucket ──
	idn := domain.IdentityName(uuid.New())
	creds, err := s.CreateIdentity(ctx, idn, a, domain.AccessReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DeleteIdentity(context.Background(), idn) }()
	cl := newClient(t, endpoint, creds.AccessKeyID, creds.SecretAccessKey)
	put := func(c *minio.Client, k string) error {
		_, err := c.PutObject(ctx, a, k, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
		return err
	}
	waitFor(t, func() error { return put(cl, "w.txt") })

	// Quota dépassé : lire et supprimer, plus écrire.
	if err := s.SetIdentityAccess(ctx, idn, a, domain.AccessReadDelete); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if put(cl, "w2.txt") == nil {
			return errStill
		}
		return nil
	})
	if err := cl.RemoveObject(ctx, a, "w.txt", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("suppression au-delà du quota : %v", err)
	}
	if _, err := cl.StatObject(ctx, a, "readme.txt", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("lecture au-delà du quota : %v", err)
	}
	// Délai de grâce : plus rien.
	if err := s.SetIdentityAccess(ctx, idn, a, domain.AccessNone); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if _, err := cl.StatObject(ctx, a, "readme.txt", minio.StatObjectOptions{}); err == nil {
			return errStill
		}
		return nil
	})
	if err := s.SetIdentityAccess(ctx, idn, a, domain.AccessReadWrite); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error { return put(cl, "w3.txt") })

	// ── Rotation : la nouvelle clé marche, l'ancienne plus ──
	next, err := s.AddAccessKey(ctx, idn)
	if err != nil {
		t.Fatal(err)
	}
	cl2 := newClient(t, endpoint, next.AccessKeyID, next.SecretAccessKey)
	waitFor(t, func() error { return put(cl2, "rotated.txt") })
	if err := s.DeleteAccessKey(ctx, idn, creds.AccessKeyID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if put(cl, "old.txt") == nil {
			return errStill
		}
		return nil
	})
	if err := put(cl2, "rotated2.txt"); err != nil {
		t.Fatalf("nouvelle clé après rotation : %v", err)
	}

	// ── Suppression d'un dossier entier ──
	if err := s.PutObject(ctx, a, "empty/", strings.NewReader(""), 0, ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteObjects(ctx, a, []string{"img/", "readme.txt", "empty/"})
	if err != nil || n != 5 { // 4 fichiers et le marqueur de empty/
		t.Fatalf("suppression : %d %v", n, err)
	}
	left, _ := s.ListObjects(ctx, a, "", "", 100)
	for _, o := range left.Objects {
		if o.Key == "img/" || o.Key == "readme.txt" || o.Key == "empty/" {
			t.Fatalf("reste : %+v", left.Objects)
		}
	}
}

func newClient(t *testing.T, endpoint, id, secret string) *minio.Client {
	t.Helper()
	cl, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(id, secret, ""), Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func httpGet(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u) //nolint:noctx // essai
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
