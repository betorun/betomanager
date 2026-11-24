package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/thekrauss/kubemanager/internal/modules/storages3/domain"
)

// ── Lecture publique ──────────────────────────────────────────────────────

// publicPolicy : lecture d'un objet par tous (Principal "*") ; lister le
// bucket reste refusé (ADR 0015, point 6).
func publicPolicy(bucket string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},`+
		`"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/*"]}]}`, bucket)
}

func (s *S3) SetPublic(ctx context.Context, bucket string, public bool) error {
	if err := guard(bucket); err != nil {
		return err
	}
	p := ""
	if public {
		p = publicPolicy(bucket)
	}
	return s.client.SetBucketPolicy(ctx, bucket, p)
}

// ── Objets ────────────────────────────────────────────────────────────────

// ListObjects : un dossier (délimiteur « / »), dossiers d'abord puis
// fichiers, au plus limit entrées après « after ».
func (s *S3) ListObjects(ctx context.Context, bucket, prefix, after string, limit int) (domain.ObjectPage, error) {
	page := domain.ObjectPage{Prefix: prefix, Objects: []domain.Object{}}
	if err := guard(bucket); err != nil {
		return page, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts := minio.ListObjectsOptions{Prefix: prefix, StartAfter: after, MaxKeys: limit}
	for o := range s.client.ListObjects(ctx, bucket, opts) {
		if o.Err != nil {
			return page, o.Err
		}
		if o.Key == prefix { // marqueur du dossier lui-même
			continue
		}
		if len(page.Objects) == limit {
			page.Next = page.Objects[len(page.Objects)-1].Key
			break
		}
		obj := domain.Object{Key: o.Key, Size: o.Size, ContentType: o.ContentType}
		if strings.HasSuffix(o.Key, "/") && o.Size == 0 && o.LastModified.IsZero() {
			obj.Dir = true
		} else if !o.LastModified.IsZero() {
			t := o.LastModified
			obj.LastModified = &t
		}
		page.Objects = append(page.Objects, obj)
	}
	sort.SliceStable(page.Objects, func(i, j int) bool {
		if page.Objects[i].Dir != page.Objects[j].Dir {
			return page.Objects[i].Dir
		}
		return page.Objects[i].Key < page.Objects[j].Key
	})
	return page, nil
}

func notFound(err error) bool {
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && (resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound)
}

func (s *S3) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, domain.Object, error) {
	if err := guard(bucket); err != nil {
		return nil, domain.Object{}, err
	}
	o, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, domain.Object{}, err
	}
	st, err := o.Stat()
	if err != nil {
		_ = o.Close()
		if notFound(err) {
			return nil, domain.Object{}, domain.ErrObjectNotFound
		}
		return nil, domain.Object{}, err
	}
	t := st.LastModified
	return o, domain.Object{Key: st.Key, Size: st.Size, ContentType: st.ContentType, LastModified: &t}, nil
}

func (s *S3) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	if err := guard(bucket); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3) DeleteObjects(ctx context.Context, bucket string, keys []string) (int, error) {
	if err := guard(bucket); err != nil {
		return 0, err
	}
	var sent atomic.Int64
	// dirs : dossiers vidés, effacés ensuite (la SeaweedFS garde les dossiers
	// vides ; le plus profond d'abord). Écrit par le producteur seulement.
	var dirs []string
	ch := make(chan minio.ObjectInfo)
	listErr := make(chan error, 1)
	send := func(k string) bool {
		select {
		case ch <- minio.ObjectInfo{Key: k}:
			sent.Add(1)
			return true
		case <-ctx.Done():
			listErr <- ctx.Err()
			return false
		}
	}
	go func() {
		defer close(ch)
		for _, k := range keys {
			if !strings.HasSuffix(k, "/") {
				if !send(k) {
					return
				}
				continue
			}
			// Dossier : tout ce qu'il contient, puis ses sous-dossiers.
			dirs = append(dirs, k)
			for o := range s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: k, Recursive: true}) {
				if o.Err != nil {
					listErr <- o.Err
					return
				}
				for i := len(k); i < len(o.Key)-1; i++ {
					if o.Key[i] == '/' {
						dirs = append(dirs, o.Key[:i+1])
					}
				}
				if !send(o.Key) {
					return
				}
			}
		}
	}()
	var errs []error
	for e := range s.client.RemoveObjects(ctx, bucket, ch, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && !notFound(e.Err) {
			errs = append(errs, fmt.Errorf("%s: %w", e.ObjectName, e.Err))
		}
	}
	for range ch { // RemoveObjects arrêté plus tôt : on libère le producteur
	}
	select {
	case err := <-listErr:
		errs = append(errs, err)
	default:
	}
	deleted := int(sent.Load()) - len(errs)
	if len(errs) == 0 {
		slices.Sort(dirs)
		dirs = slices.Compact(dirs)
		slices.SortStableFunc(dirs, func(x, y string) int { return strings.Count(y, "/") - strings.Count(x, "/") })
		for _, d := range dirs {
			if err := s.client.RemoveObject(ctx, bucket, d, minio.RemoveObjectOptions{}); err != nil && !notFound(err) {
				errs = append(errs, fmt.Errorf("%s: %w", d, err))
			}
		}
	}
	return deleted, errors.Join(errs...)
}

func (s *S3) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	if err := guard(bucket); err != nil {
		return "", err
	}
	// Le nom affiché au téléchargement : la fin du chemin.
	name := key[strings.LastIndex(key, "/")+1:]
	params := url.Values{}
	params.Set("response-content-disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name)))
	u, err := s.public.PresignedGetObject(ctx, bucket, key, ttl, params)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
