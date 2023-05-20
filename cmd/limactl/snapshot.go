package main

import (
	"fmt"
	"strings"

	"github.com/lima-vm/lima/pkg/snapshot"
	"github.com/lima-vm/lima/pkg/store"

	"github.com/spf13/cobra"
)

func newSnapshotCommand() *cobra.Command {
	var snapshotCmd = &cobra.Command{
		Use:   "snapshot",
		Short: "Manage instance snapshots",
	}
	snapshotCmd.AddCommand(newSnapshotApplyCommand())
	snapshotCmd.AddCommand(newSnapshotCreateCommand())
	snapshotCmd.AddCommand(newSnapshotDeleteCommand())
	snapshotCmd.AddCommand(newSnapshotListCommand())

	return snapshotCmd
}

func newSnapshotCreateCommand() *cobra.Command {
	var createCmd = &cobra.Command{
		Use:               "create INSTANCE",
		Aliases:           []string{"save"},
		Short:             "Create (save) a snapshot",
		Args:              cobra.MinimumNArgs(1),
		RunE:              snapshotCreateAction,
		ValidArgsFunction: snapshotBashComplete,
	}
	createCmd.Flags().String("tag", "", "name of the snapshot")

	return createCmd
}

func snapshotCreateAction(cmd *cobra.Command, args []string) error {
	instName := args[0]

	inst, err := store.Inspect(instName)
	if err != nil {
		return err
	}

	tag, err := cmd.Flags().GetString("tag")
	if err != nil {
		return err
	}

	if tag == "" {
		return fmt.Errorf("expected tag")
	}

	ctx := cmd.Context()
	return snapshot.Save(ctx, inst, tag)
}

func newSnapshotDeleteCommand() *cobra.Command {
	var deleteCmd = &cobra.Command{
		Use:               "delete INSTANCE",
		Aliases:           []string{"del"},
		Short:             "Delete (del) a snapshot",
		Args:              cobra.MinimumNArgs(1),
		RunE:              snapshotDeleteAction,
		ValidArgsFunction: snapshotBashComplete,
	}
	deleteCmd.Flags().String("tag", "", "name of the snapshot")

	return deleteCmd
}

func snapshotDeleteAction(cmd *cobra.Command, args []string) error {
	instName := args[0]

	inst, err := store.Inspect(instName)
	if err != nil {
		return err
	}

	tag, err := cmd.Flags().GetString("tag")
	if err != nil {
		return err
	}

	if tag == "" {
		return fmt.Errorf("expected tag")
	}

	ctx := cmd.Context()
	return snapshot.Del(ctx, inst, tag)
}

func newSnapshotApplyCommand() *cobra.Command {
	var applyCmd = &cobra.Command{
		Use:               "apply INSTANCE",
		Aliases:           []string{"load"},
		Short:             "Apply (load) a snapshot",
		Args:              cobra.MinimumNArgs(1),
		RunE:              snapshotApplyAction,
		ValidArgsFunction: snapshotBashComplete,
	}
	applyCmd.Flags().String("tag", "", "name of the snapshot")

	return applyCmd
}

func snapshotApplyAction(cmd *cobra.Command, args []string) error {
	instName := args[0]

	inst, err := store.Inspect(instName)
	if err != nil {
		return err
	}

	tag, err := cmd.Flags().GetString("tag")
	if err != nil {
		return err
	}

	if tag == "" {
		return fmt.Errorf("expected tag")
	}

	ctx := cmd.Context()
	return snapshot.Load(ctx, inst, tag)
}

func newSnapshotListCommand() *cobra.Command {
	var listCmd = &cobra.Command{
		Use:               "list INSTANCE",
		Aliases:           []string{"ls"},
		Short:             "List existing snapshots",
		Args:              cobra.MinimumNArgs(1),
		RunE:              snapshotListAction,
		ValidArgsFunction: snapshotBashComplete,
	}
	listCmd.Flags().BoolP("quiet", "q", false, "Only show tags")

	return listCmd
}

func snapshotListAction(cmd *cobra.Command, args []string) error {
	instName := args[0]

	inst, err := store.Inspect(instName)
	if err != nil {
		return err
	}

	quiet, err := cmd.Flags().GetBool("quiet")
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out, err := snapshot.List(ctx, inst)
	if err != nil {
		return err
	}
	if quiet {
		for i, line := range strings.Split(out, "\n") {
			// "ID", "TAG", "VM SIZE", "DATE", "VM CLOCK", "ICOUNT"
			fields := strings.Fields(line)
			if i == 0 && len(fields) > 1 && fields[1] != "TAG" {
				// make sure that output matches the expected
				return fmt.Errorf("unknown header: %s", line)
			}
			if i == 0 || line == "" {
				// skip header and empty line after using split
				continue
			}
			tag := fields[1]
			fmt.Printf("%s\n", tag)
		}
		return nil
	}
	fmt.Print(out)
	return nil
}

func snapshotBashComplete(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return bashCompleteInstanceNames(cmd)
}
