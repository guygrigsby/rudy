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
func newModelsCommand(build buildFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "models",
		Short: "list the discovered models with context windows and prices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := build(cmd.Context(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer func() { _ = b.Server.Shutdown(context.Background()) }()
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
	_, _ = fmt.Fprintln(tw, "PROVIDER\tMODEL\tCONTEXT\tIN/1M\tOUT/1M\tNAME")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Ref.Provider, m.Ref.Model, intOrDash(m.ContextWindow),
			per1M(m.Pricing.Input), per1M(m.Pricing.Output), m.DisplayName)
	}
	return tw.Flush()
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
