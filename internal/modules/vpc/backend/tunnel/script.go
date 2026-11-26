package tunnel

import (
	_ "embed"
	"net/url"
	"regexp"
	"strings"

	"github.com/thekrauss/kubemanager/internal/modules/vpc/domain"
)

//go:embed enroll.sh
var enrollScript string

var _ domain.Scripter = (*Backend)(nil)

// tokenRe : forme d'un jeton d'inscription (rien d'autre n'entre dans le script).
var tokenRe = regexp.MustCompile(`^bvpc_[A-Za-z0-9_-]{20,100}$`)

// EnrollScript : script d'inscription (POSIX sh, Debian et Ubuntu). token
// vide : lu en premier argument (script servi aux VPS existants). "" si
// l'URL ou le jeton sont mal formés.
func (b *Backend) EnrollScript(apiURL, token string) string {
	u, err := url.Parse(apiURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(apiURL, "'\"`$\\ \n") {
		return ""
	}
	if token != "" && !tokenRe.MatchString(token) {
		return ""
	}
	return strings.NewReplacer("__API__", strings.TrimRight(apiURL, "/"), "__TOKEN__", token).Replace(enrollScript)
}
