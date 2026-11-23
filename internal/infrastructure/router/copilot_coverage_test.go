package router

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Couverture du Copilot : chaque route publique a une décision (outil, guide
// ou hors Copilot), chaque décision vise une route qui existe, chaque guide un
// écran réel, et aucun outil ne vise une route qui refuse le jeton délégué.
// Une fonctionnalité nouvelle sans décision casse ce test : le Copilot ne
// peut ni l'ignorer ni en connaître une disparue.
func TestCopilotCoverage(t *testing.T) {
	documentedAPI(t)
	problems := copilotCoverageProblems(AllGroups)
	require.Empty(t, problems, "\n"+strings.Join(problems, "\n"))

	tools, guides, out := 0, 0, 0
	walkRoutes(AllGroups, func(full string, r *Route, internal bool) {
		if internal {
			return
		}
		switch {
		case r.Copilot != nil:
			tools++
		case r.CopilotGuide != nil:
			guides++
		default:
			out++
		}
	})
	t.Logf("routes publiques : %d outils, %d guides, %d hors Copilot", tools, guides, out)
	require.Len(t, CopilotGuideRoutes(AllGroups), guides)
}
