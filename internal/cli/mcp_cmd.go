package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thekrauss/kubemanager/internal/cli/i18n"
	"github.com/thekrauss/kubemanager/internal/cli/mcp"
)

// newMCP : « betorun mcp » : serveur MCP sur stdin/stdout, lancé par un agent
// de code (Claude Code, Cursor, Windsurf, VS Code). Il utilise les mêmes
// identifiants que la CLI ; rien n'est écrit sur la sortie standard en dehors
// du protocole.
func newMCP() *cobra.Command {
	return &cobra.Command{
		Use:     "mcp",
		Short:   i18n.T("mcp.short"),
		Long:    i18n.T("mcp.long"),
		Args:    cobra.NoArgs,
		Example: "claude mcp add betorun -- betorun mcp",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := defaultMCPEnv()
			srv := &mcp.Server{
				Name: "betorun", Version: Version, Instructions: mcpInstructions, Tools: env.tools(),
				Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
			}
			return srv.Run(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
