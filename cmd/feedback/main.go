// Command feedback runs the feedback operator, which reconciles the
// feedback.miloapis.com CRDs in Milo and serves their admission webhooks.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.miloapis.com/feedback/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feedback",
		Short: "Feedback - roadmap, votes and submissions for Milo",
		Long: `Feedback reconciles the feedback.miloapis.com resources in Milo: public
roadmap posts synced from GitHub, votes, submissions and security reports.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newOperatorCommand(), newVersionCommand())
	return cmd
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(*cobra.Command, []string) {
			i := version.Get()
			fmt.Printf("Feedback\n")
			fmt.Printf("  Version:       %s\n", i.Version)
			fmt.Printf("  Git Commit:    %s\n", i.GitCommit)
			fmt.Printf("  Git Tree:      %s\n", i.GitTreeState)
			fmt.Printf("  Build Date:    %s\n", i.BuildDate)
			fmt.Printf("  Go Version:    %s\n", i.GoVersion)
			fmt.Printf("  Platform:      %s\n", i.Platform)
		},
	}
}
