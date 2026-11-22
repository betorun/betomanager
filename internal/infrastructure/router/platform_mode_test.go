package router

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/wI2L/fizz"
)

func TestServedIn(t *testing.T) {
	for _, c := range []struct {
		path             string
		server, platform bool
	}{
		{"/admin/billing", false, true},
		{"/admin/cluster-keys", false, true},
		{"/identity", true, true},
		{"/iam", true, true},
		{"/projects/:id/workloads", true, false},
		{"/organizations/:id/domains", true, false},
		{"/administrators", true, false}, // préfixe trompeur : pas /admin/
	} {
		assert.Equal(t, c.server, ServedIn(ModeServer, c.path), "server %s", c.path)
		assert.Equal(t, c.platform, ServedIn(ModePlatform, c.path), "platform %s", c.path)
		assert.True(t, ServedIn("", c.path))
	}
}

// Toute route staff (permission platform.*, hors watchers des clusters) est
// sous /admin : l'API des clients ne la sert donc pas.
func TestPlatformRoutesLiveUnderAdmin(t *testing.T) {
	documentedAPI(t)
	for _, root := range AllGroups {
		for _, g := range root.Groups {
			var walk func(grp *RouteGroup, abs string)
			walk = func(grp *RouteGroup, abs string) {
				for _, r := range grp.Routes {
					p := string(r.Access.permission)
					if strings.HasPrefix(p, "platform.") && r.Access.kind != accessClusterAgent {
						assert.True(t, strings.HasPrefix(g.Path, "/admin/"), "%s %s (%s) doit être sous /admin", r.Method, abs+r.Path, p)
					}
				}
				for _, c := range grp.Groups {
					walk(c, abs+c.Path)
				}
			}
			walk(g, root.Path+g.Path)
		}
	}
}

// Routes réellement montées par chaque mode.
func TestModesMountTheirRoutes(t *testing.T) {
	documentedAPI(t)
	defer func() { servingMode = "" }()
	mounted := func(mode Mode) []gin.RouteInfo {
		engine := gin.New()
		RegisterRoutesFor(fizz.NewFromEngine(engine), mode)
		return engine.Routes()
	}
	server, platform := mounted(ModeServer), mounted(ModePlatform)
	assert.NotEmpty(t, server)
	assert.NotEmpty(t, platform)
	for _, r := range server {
		assert.NotContains(t, r.Path, PathAPIRoot+"/admin/", "l'API des clients ne sert pas %s %s", r.Method, r.Path)
	}
	sawAdmin := false
	for _, r := range platform {
		rel := strings.TrimPrefix(r.Path, PathAPIRoot)
		ok := strings.HasPrefix(rel, "/admin/") || strings.HasPrefix(rel, "/identity") || strings.HasPrefix(rel, "/iam")
		assert.True(t, ok, "l'API plateforme ne sert pas %s %s", r.Method, r.Path)
		sawAdmin = sawAdmin || strings.HasPrefix(rel, "/admin/")
	}
	assert.True(t, sawAdmin)
}
