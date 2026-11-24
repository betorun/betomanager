package repository

// Essai sur un vrai S3 (SeaweedFS de dev, IAM en écriture) :
//
//	STORAGE_S3_E2E=1 go test ./internal/modules/storages3/repository/ -run E2E -v
//
// Identité d'administration : « storage » de docker/seaweedfs/s3.json
// (surchargeable par STORAGE_S3_ENDPOINT, STORAGE_S3_ACCESS_KEY, STORAGE_S3_SECRET_KEY).

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestS3_E2E(t *testing.T) {
	if os.Getenv("STORAGE_S3_E2E") != "1" {
		t.Skip("STORAGE_S3_E2E=1 pour l'essai sur un vrai S3")
	}
	ctx := context.Background()
	endpoint := env("STORAGE_S3_ENDPOINT", "127.0.0.1:8333")
	s, err := NewS3(S3Config{Endpoint: endpoint, AccessKey: env("STORAGE_S3_ACCESS_KEY", "betorun-storage-dev"),
		SecretKey: env("STORAGE_S3_SECRET_KEY", "storage-dev-secret")})
	if err != nil {
		t.Fatal(err)
	}
	a, b := domain.NewBucketName(), domain.NewBucketName()
	for _, n := range []string{a, b} {
		if err := s.CreateBucket(ctx, n); err != nil {
			t.Fatal(err)
		}
		defer func(n string) { _ = s.DeleteBucket(context.Background(), n, true) }(n)
	}
	if err := s.CreateBucket(ctx, "betorun-sites"); err == nil {
		t.Fatal("bucket hors préfixe accepté")
	}
	rw := domain.IdentityName(uuid.New())
	creds, err := s.CreateIdentity(ctx, rw, a, domain.AccessReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.DeleteIdentity(context.Background(), rw) }()
	client := func(c struct{ id, secret string }) *minio.Client {
		cl, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(c.id, c.secret, ""), Region: "us-east-1"})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	cl := client(struct{ id, secret string }{creds.AccessKeyID, creds.SecretAccessKey})
	put := func(bucket string) error {
		_, err := cl.PutObject(ctx, bucket, "hello.txt", bytes.NewReader([]byte("bonjour")), 7, minio.PutObjectOptions{})
		return err
	}
	// La SeaweedFS recharge sa configuration IAM de façon asynchrone.
	waitFor(t, func() error { return put(a) })
	if err := put(b); err == nil {
		t.Fatal("écriture dans un autre bucket acceptée")
	}
	// Lister les buckets ne révèle jamais ceux des autres.
	if list, err := cl.ListBuckets(ctx); err == nil {
		for _, bi := range list {
			if bi.Name != a {
				t.Fatalf("bucket d'un autre visible : %s", bi.Name)
			}
		}
	}
	if _, err := cl.StatObject(ctx, "betorun-sites", "index.html", minio.StatObjectOptions{}); err == nil {
		t.Fatal("lecture du bucket des sites acceptée")
	}
	used, err := s.Usage(ctx, a)
	if err != nil || used != 7 {
		t.Fatalf("usage : %d %v", used, err)
	}
	// Lecture seule : lire oui, écrire non.
	if err := s.SetIdentityAccess(ctx, rw, a, domain.AccessReadOnly); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if put(a) == nil {
			return errStill
		}
		return nil
	})
	if _, err := cl.StatObject(ctx, a, "hello.txt", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("lecture en lecture seule : %v", err)
	}
	if err := s.DeleteBucket(ctx, a, false); err != domain.ErrNotEmpty {
		t.Fatalf("bucket non vide : %v", err)
	}
	// Révocation : la clé ne sert plus.
	if err := s.DeleteIdentity(ctx, rw); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if _, err := cl.StatObject(ctx, a, "hello.txt", minio.StatObjectOptions{}); err == nil {
			return errStill
		}
		return nil
	})
	if err := s.DeleteIdentity(ctx, rw); err != nil {
		t.Fatalf("seconde suppression : %v", err)
	}
	if err := s.DeleteBucket(ctx, a, true); err != nil {
		t.Fatal(err)
	}
}
