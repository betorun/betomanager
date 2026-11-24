package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7/pkg/signer"
)

// iamClient : API IAM compatible AWS (protocole « query », signé SigV4 pour
// le service iam). Seules les actions utiles au module.
type iamClient struct {
	endpoint  string // URL complète, ex. http://127.0.0.1:8333
	region    string
	accessKey string
	secretKey string
	http      *http.Client
}

// iamError : erreur renvoyée par l'API IAM.
type iamError struct {
	Status  int
	Code    string
	Message string
}

func (e *iamError) Error() string { return fmt.Sprintf("iam %d %s: %s", e.Status, e.Code, e.Message) }

func isNoSuchEntity(err error) bool {
	e, ok := err.(*iamError)
	return ok && (e.Code == "NoSuchEntity" || e.Status == http.StatusNotFound)
}

func (c *iamClient) do(ctx context.Context, action string, params url.Values, out any) error {
	v := url.Values{"Action": {action}, "Version": {"2010-05-08"}}
	for k, vals := range params {
		v[k] = vals
	}
	body := v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	sum := sha256.Sum256([]byte(body))
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	req = signer.SignV4WithServiceType(*req, c.accessKey, c.secretKey, "", c.region, "iam")
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		}
		_ = xml.Unmarshal(raw, &e)
		return &iamError{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out != nil {
		return xml.Unmarshal(raw, out)
	}
	return nil
}

func (c *iamClient) createUser(ctx context.Context, user string) error {
	return c.do(ctx, "CreateUser", url.Values{"UserName": {user}}, nil)
}

func (c *iamClient) deleteUser(ctx context.Context, user string) error {
	return c.do(ctx, "DeleteUser", url.Values{"UserName": {user}}, nil)
}

func (c *iamClient) createAccessKey(ctx context.Context, user string) (id, secret string, err error) {
	var out struct {
		Result struct {
			AccessKey struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
			} `xml:"AccessKey"`
		} `xml:"CreateAccessKeyResult"`
	}
	if err := c.do(ctx, "CreateAccessKey", url.Values{"UserName": {user}}, &out); err != nil {
		return "", "", err
	}
	k := out.Result.AccessKey
	if k.AccessKeyID == "" || k.SecretAccessKey == "" {
		return "", "", fmt.Errorf("iam: empty access key")
	}
	return k.AccessKeyID, k.SecretAccessKey, nil
}

func (c *iamClient) listAccessKeys(ctx context.Context, user string) ([]string, error) {
	var out struct {
		Result struct {
			Members []struct {
				AccessKeyID string `xml:"AccessKeyId"`
			} `xml:"AccessKeyMetadata>member"`
		} `xml:"ListAccessKeysResult"`
	}
	if err := c.do(ctx, "ListAccessKeys", url.Values{"UserName": {user}}, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Result.Members))
	for _, m := range out.Result.Members {
		ids = append(ids, m.AccessKeyID)
	}
	return ids, nil
}

func (c *iamClient) deleteAccessKey(ctx context.Context, user, id string) error {
	return c.do(ctx, "DeleteAccessKey", url.Values{"UserName": {user}, "AccessKeyId": {id}}, nil)
}

func (c *iamClient) putUserPolicy(ctx context.Context, user, name string, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return c.do(ctx, "PutUserPolicy", url.Values{"UserName": {user}, "PolicyName": {name}, "PolicyDocument": {string(raw)}}, nil)
}

func (c *iamClient) deleteUserPolicy(ctx context.Context, user, name string) error {
	return c.do(ctx, "DeleteUserPolicy", url.Values{"UserName": {user}, "PolicyName": {name}}, nil)
}
