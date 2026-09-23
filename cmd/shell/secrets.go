package main

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	secrets "github.com/rcliao/shell-secrets"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/decide"
)

// shell secrets doctor — for every secret this agent's config refers to,
// say where it resolves from (store, environment) or that it is missing,
// and what a Claude child will see. Values are never printed.
func newSecretsCmd() *cobra.Command {
	var configFlag string
	cmd := &cobra.Command{Use: "secrets", Short: "Inspect how this agent resolves its secrets"}
	cmd.PersistentFlags().StringVar(&configFlag, "config", "", "agent config path (default ~/.shell/config.json)")

	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Report each configured secret's source and the child-env policy (never prints values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadConfigFrom(configFlag)
			out := cmd.OutOrStdout()
			problems := 0

			fmt.Fprintf(out, "store: enabled=%t\n", cfg.Secrets.Enabled)
			if cfg.Secrets.Enabled {
				config.OpenSecretStore(cfg.Secrets)
				defer config.CloseSecretStore()
				if config.SecretStoreOpen() {
					fmt.Fprintf(out, "  ✓ open, %d managed name(s)\n", len(config.ManagedSecretNames()))
				} else {
					problems++
					_, err := secrets.NewStore(cfg.Secrets.StorePath)
					fmt.Fprintf(out, "  ✗ not open: %v\n", err)
					if errors.Is(err, secrets.ErrV1Store) {
						fmt.Fprintln(out, "  → run `shell-secrets migrate --from-v1` in a Terminal window (see shell-secrets doctor)")
					}
				}
			} else {
				fmt.Fprintln(out, "  (secrets resolve from the environment only)")
			}

			notion := cfg.Notion.TokenSecret
			if notion == "" {
				notion = "NOTION_TOKEN"
			}
			refs := []struct{ what, name string }{
				{"telegram bot token", cfg.Telegram.TokenEnv},
				{"notion token", notion},
				{"jev key (shadow router)", decide.KeyName},
			}
			fmt.Fprintln(out, "references:")
			for _, r := range refs {
				src := secretSource(cfg, r.name)
				mark := "✓"
				if src == "missing" {
					mark = "✗"
					if r.what != "jev key (shadow router)" { // optional feature
						problems++
					}
				}
				fmt.Fprintf(out, "  %s %-26s %-24s %s\n", mark, r.what, r.name, src)
			}

			pass := cfg.SecretPassthrough()
			names := cfg.Secrets.Passthrough
			if names == nil {
				names = config.DefaultSecretPassthrough
			}
			fmt.Fprintln(out, "child env passthrough (what skill binaries see):")
			for _, n := range names {
				if _, ok := pass[n]; ok {
					fmt.Fprintf(out, "  ✓ %-24s %s\n", n, secretSource(cfg, n))
				} else {
					fmt.Fprintf(out, "  – %-24s not set (skill using it will fail)\n", n)
				}
			}
			// What a child will NOT see: managed + referenced names, minus the
			// passthrough (applied after stripping), deduplicated.
			seen := map[string]bool{}
			var stripped []string
			for _, n := range append(config.ManagedSecretNames(), cfg.Telegram.TokenEnv, notion, decide.KeyName) {
				if _, passes := pass[n]; passes || seen[n] || n == "" {
					continue
				}
				seen[n] = true
				stripped = append(stripped, n)
			}
			sort.Strings(stripped)
			fmt.Fprintf(out, "stripped from child env: %v plus any *_BOT_TOKEN\n", stripped)

			if problems > 0 {
				cmd.SilenceUsage = true
				return fmt.Errorf("%d problem(s)", problems)
			}
			fmt.Fprintln(out, "all good")
			return nil
		},
	}
	cmd.AddCommand(doctor)
	return cmd
}

// secretSource says where a name resolves from, without revealing it.
func secretSource(cfg config.Config, name string) string {
	if name == "" {
		return "missing (no name configured)"
	}
	if config.SecretStoreOpen() {
		for _, k := range config.ManagedSecretNames() {
			if k == name {
				return "store"
			}
		}
	}
	if os.Getenv(name) != "" {
		return "environment"
	}
	return "missing"
}
