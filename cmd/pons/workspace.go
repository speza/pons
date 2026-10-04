package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/httptransport"
)

const workspaceUsage = `usage:
  pons workspace checkpoints [-server URL] [-workspace-id ID]
  pons workspace restore     [-server URL] [-workspace-id ID] SEQ`

// runWorkspace implements `pons workspace checkpoints|restore`, thin clients
// of the server's checkpoint API. They never open the state directory.
func runWorkspace(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "checkpoints" && args[0] != "restore") {
		fmt.Fprintln(stderr, workspaceUsage)
		return errors.New("a workspace command, checkpoints or restore, is required")
	}
	command := args[0]
	flags := flag.NewFlagSet("pons workspace "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	serverURL := flags.String("server", "http://127.0.0.1:7337", "runtime server URL")
	workspaceID := flags.String("workspace-id", "", "workspace ID (default: the server agent's workspace)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, workspaceUsage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	wantArgs := 0
	if command == "restore" {
		wantArgs = 1
	}
	if flags.NArg() != wantArgs {
		flags.Usage()
		if command == "restore" {
			return errors.New("exactly one checkpoint SEQ is required")
		}
		return errors.New("checkpoints takes no arguments")
	}
	var seq int64
	if command == "restore" {
		var err error
		seq, err = strconv.ParseInt(flags.Arg(0), 10, 64)
		if err != nil || seq <= 0 {
			return fmt.Errorf("checkpoint SEQ %q must be a positive integer", flags.Arg(0))
		}
	}

	client := httptransport.Client{BaseURL: *serverURL}
	id := *workspaceID
	if id == "" {
		options, err := client.RuntimeOptions(ctx)
		if err != nil {
			return err
		}
		if options.Agent.ID == "" {
			return errors.New("server did not report its agent; pass -workspace-id")
		}
		id = ponsruntime.AgentWorkspaceID(options.Agent.ID)
	}

	if command == "checkpoints" {
		list, err := client.WorkspaceCheckpoints(ctx, id)
		if err != nil {
			return err
		}
		printCheckpoints(stdout, list)
		return nil
	}
	restored, err := client.RestoreWorkspace(ctx, id, seq)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restored checkpoint %d of %s as checkpoint %d\n", seq, id, restored.Seq)
	return nil
}

func printCheckpoints(out io.Writer, list ponsruntime.WorkspaceCheckpoints) {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "SEQ\tKIND\tCREATED\tSIZE\tDETAIL")
	for _, checkpoint := range list.Checkpoints {
		var detail string
		switch {
		case checkpoint.RestoredFrom != 0:
			detail = fmt.Sprintf("restored from %d", checkpoint.RestoredFrom)
		case checkpoint.RunID != "":
			detail = "run " + checkpoint.RunID
		}
		if checkpoint.Current {
			detail = joinDetail(detail, "current")
		}
		fmt.Fprintf(writer, "%d\t%s\t%s\t%d\t%s\n", checkpoint.Seq, checkpoint.Kind,
			checkpoint.CreatedAt.Local().Format(time.DateTime), checkpoint.SizeBytes, detail)
	}
	_ = writer.Flush()
}

func joinDetail(detail, more string) string {
	if detail == "" {
		return more
	}
	return detail + ", " + more
}
