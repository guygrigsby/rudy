package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/session"
)

// newSessionsCommand is the "sessions" noun with the list, resume and fork verbs under it.
func newSessionsCommand(build buildFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "list and manage sessions",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "list sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			st, err := storeFromEnv(os.Getenv, home)
			if err != nil {
				return err
			}
			summaries, err := st.List()
			if err != nil {
				return err
			}
			return renderSessions(cmd.OutOrStdout(), summaries)
		},
	}
	var resumeDial dialOptions
	resume := &cobra.Command{
		Use:   "resume <session id>",
		Short: "open the client on an existing session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			return tuiExit(runTUI(cmd.Context(), build, resumeDial, resumeWith(printOptions{Resume: args[0]}, "sessions resume"), launchTUI, "", stderr))
		},
	}
	registerDialFlags(resume, &resumeDial)
	var at string
	var forkDial dialOptions
	fork := &cobra.Command{
		Use:   "fork <session id>",
		Short: "open the client on a fork of an existing session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stderr := cmd.ErrOrStderr()
			return tuiExit(runTUI(cmd.Context(), build, forkDial, forkAt(args[0], at), launchTUI, "", stderr))
		},
	}
	registerDialFlags(fork, &forkDial)
	fork.Flags().StringVar(&at, "at", "", "entry id to fork at; the newest entry by default")
	cmd.AddCommand(list, resume, fork)
	return cmd
}

// renderSessions prints one row per session in the order Store.List returns them (newest first).
func renderSessions(w io.Writer, summaries []session.Summary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tOPENED\tWORKSPACE\tMODEL\tFORKED\tPARENT")
	for _, s := range summaries {
		forked := ""
		if s.Forked {
			forked = "yes"
		}
		// A child session shows enough of its parent's id to find it in this same list.
		parent := s.ParentSessionID
		if len(parent) > 8 {
			parent = parent[:8]
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID.String(), s.OpenedAt.Local().Format("2006-01-02 15:04"), s.Workspace.Root, s.Model.String(), forked, parent)
	}
	return tw.Flush()
}
