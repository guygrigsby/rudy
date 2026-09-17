// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/guygrigsby/rudy/internal/provider"
)

// newModelsCommand builds the models list. It runs the full wiring (build), which refreshes
// the registry, so the table always reflects a fresh /v1/models call.
// newModelsCommand is `rudy models`, a noun whose verbs do the work: the CLI is noun then
// verb, and a bare noun prints its help (ADR 0022).
func newModelsCommand(build buildFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "the model registry",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newModelsListCommand(build))
	return cmd
}

func newModelsListCommand(build buildFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list the discovered models with context windows and prices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := build(cmd.Context(), BuildOptions{Stderr: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			defer func() { _ = b.Close(context.Background()) }()
			models := b.Registry.Models()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(models)
			}
			return renderModels(cmd.OutOrStdout(), models)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the registry as JSON")
	return cmd
}

// renderModels prints one row per model. Prices are per million tokens.
func renderModels(w io.Writer, models []provider.Model) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// UPSTREAM before the price: an endpoint that fronts several serves the same id from
	// more than one of them, and without this column the rows read as duplicates.
	_, _ = fmt.Fprintln(tw, "PROVIDER\tMODEL\tUPSTREAM\tCONTEXT\tIN/1M\tOUT/1M\tNAME")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Ref.Provider, m.Ref.Model, dashIfEmpty(m.Upstream), intOrDash(m.ContextWindow),
			per1M(m.Pricing.Input), per1M(m.Pricing.Output), m.DisplayName)
	}
	return tw.Flush()
}

// dashIfEmpty is a column an endpoint said nothing about.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// per1M turns a per-token decimal string into dollars per million tokens, or "-" when unknown.
func per1M(perToken string) string {
	if perToken == "" {
		return "-"
	}
	r, ok := new(big.Rat).SetString(perToken)
	if !ok {
		return "-"
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	return "$" + r.FloatString(2)
}

func intOrDash(n int64) string {
	if n == 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}
