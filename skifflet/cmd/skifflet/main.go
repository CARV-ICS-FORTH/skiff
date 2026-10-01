// Copyright © 2022 FORTH-ICS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"skifflet/cmd/skifflet/commands"
	"skifflet/cmd/skifflet/commands/root"
	"skifflet/internal/compute/podhandler"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// NewRootCommand creates a new providers subcommand
// This subcommand is used to determine which providers are registered.
func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "Show the list of supported providers",
		Long:  "Show the list of supported providers",
		Args:  cobra.MaximumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			switch len(args) {
			case 0:
				fmt.Fprintln(cmd.OutOrStdout(), "skiff")
			case 1:
				if args[0] != "skiff" {
					fmt.Fprintln(cmd.OutOrStderr(), "no such provider", args[0])

					os.Exit(1)
				}
				fmt.Fprintln(cmd.OutOrStdout(), args[0])
			}
			return
		},
	}
}

// NewVersionCommand creates a new version subcommand command
func NewVersionCommand(version, buildTime, gitCommit string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the version of the program",
		Long:  `Show the version of the program`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("skifflet version: %s (commit: %s, built: %s)\n", version, gitCommit, buildTime)
		},
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		// The node is told to stop (the job ends): its pods stop first, by their own
		// termination sequence (PreStop hooks, SIGTERM, their grace period, within what
		// the job's end allows), and only then does the rest.
		if n := podhandler.GlobalRegistry.DrainAll(podhandler.DrainBudget); n > 0 {
			log.G(ctx).Infof("stopped the %d pod(s) of this node", n)
		}
		cancel()
	}()

	var opts root.Opts

	rootCmd := root.NewCommand(ctx, filepath.Base(os.Args[0]), opts)
	rootCmd.AddCommand(NewVersionCommand(commands.BuildVersion, commands.BuildTime, commands.GitCommit), NewRootCommand())
	preRun := rootCmd.PreRunE

	var logLevel string
	rootCmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if preRun != nil {
			return preRun(cmd, args)
		}

		return nil
	}

	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", `set the log level, e.g. "debug", "info", "warn", "error"`)

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if logLevel != "" {
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				return fmt.Errorf("could not parse log level: %w", err)
			}
			logrus.SetLevel(lvl)
		}

		return nil
	}

	if err := rootCmd.Execute(); err != nil && !errors.Is(err, context.Canceled) {
		log.G(ctx).Fatal(err)
	}
}
