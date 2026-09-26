package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// runMonitor dispatches the monitor command group.
func runMonitor(ctx context.Context, env *cliEnv, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(env.out, monitorUsage)
		return errHelp
	}
	switch args[0] {
	case "list":
		return monitorList(ctx, env, args[1:])
	case "get":
		return monitorGet(ctx, env, args[1:])
	case "create":
		return monitorCreate(ctx, env, args[1:])
	case "delete":
		return monitorDelete(ctx, env, args[1:])
	case "enable":
		return monitorSetEnabled(ctx, env, args[1:], true)
	case "disable":
		return monitorSetEnabled(ctx, env, args[1:], false)
	case "help", "-h", "--help":
		fmt.Fprint(env.out, monitorUsage)
		return errHelp
	default:
		return fmt.Errorf("unknown monitor command %q; run 'sorobeacon monitor' for usage", args[0])
	}
}

func monitorList(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("monitor list")
	enabled := fs.Bool("enabled", false, "only enabled monitors")
	disabled := fs.Bool("disabled", false, "only disabled monitors")
	query := fs.String("q", "", "filter by name or contract id")
	limit := fs.Int("limit", 0, "page size (server default 50, max 500)")
	if err := parseFlags(fs, args, monitorUsage, env.out); err != nil {
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if *enabled && *disabled {
		return errors.New("--enabled and --disabled are mutually exclusive")
	}

	opts := apiclient.ListOptions{Query: *query, Limit: *limit}
	switch {
	case *enabled:
		opts.Enabled = boolPtr(true)
	case *disabled:
		opts.Enabled = boolPtr(false)
	}
	monitors, err := env.client.ListMonitors(ctx, opts)
	if err != nil {
		return err
	}
	return env.printMonitorList(monitors)
}

func monitorGet(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("monitor get")
	if err := parseFlags(fs, args, monitorUsage, env.out); err != nil {
		return err
	}
	id, err := singleIDArg(fs, "sorobeacon monitor get ID")
	if err != nil {
		return err
	}
	m, err := env.client.GetMonitor(ctx, id)
	if err != nil {
		return err
	}
	return env.printMonitor(m)
}

// monitorCreate creates a monitor and prints it as stored, so a script can
// pick the new id out of the JSON without a second lookup.
func monitorCreate(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("monitor create")
	name := fs.String("name", "", "monitor name (required)")
	var contracts stringList
	var channels int64List
	fs.Var(&contracts, "contract", "contract id to watch (repeatable, required)")
	fs.Var(&channels, "channel", "channel id to alert to (repeatable)")
	disabled := fs.Bool("disabled", false, "create the monitor disabled")
	if err := parseFlags(fs, args, monitorUsage, env.out); err != nil {
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	if len(contracts) == 0 {
		return errors.New("at least one --contract is required")
	}

	in := apiclient.MonitorCreate{Name: *name, ContractIDs: contracts, ChannelIDs: channels}
	if *disabled {
		in.Enabled = boolPtr(false)
	}
	m, err := env.client.CreateMonitor(ctx, in)
	if err != nil {
		return err
	}
	return env.printMonitor(m)
}

func monitorDelete(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("monitor delete")
	if err := parseFlags(fs, args, monitorUsage, env.out); err != nil {
		return err
	}
	id, err := singleIDArg(fs, "sorobeacon monitor delete ID")
	if err != nil {
		return err
	}
	if err := env.client.DeleteMonitor(ctx, id); err != nil {
		return err
	}
	return env.printDeleted("monitor", id)
}

// monitorSetEnabled backs both `enable` and `disable`: the API has no
// dedicated routes for them, so both are a one-field PATCH.
func monitorSetEnabled(ctx context.Context, env *cliEnv, args []string, enabled bool) error {
	command := "disable"
	if enabled {
		command = "enable"
	}
	fs := newFlagSet("monitor " + command)
	if err := parseFlags(fs, args, monitorUsage, env.out); err != nil {
		return err
	}
	id, err := singleIDArg(fs, fmt.Sprintf("sorobeacon monitor %s ID", command))
	if err != nil {
		return err
	}
	m, err := env.client.UpdateMonitor(ctx, id, apiclient.MonitorUpdate{Enabled: &enabled})
	if err != nil {
		return err
	}
	return env.printMonitor(m)
}

// singleIDArg reads the one id a command takes and rejects anything else, so
// "monitor get 1 extra" is a usage error rather than a silent no-op.
func singleIDArg(fs *flag.FlagSet, usage string) (int64, error) {
	if fs.NArg() != 1 {
		return 0, fmt.Errorf("usage: %s", usage)
	}
	return parseID(fs.Arg(0))
}

func (e *cliEnv) printMonitorList(monitors []apiclient.Monitor) error {
	if e.json {
		if monitors == nil {
			monitors = []apiclient.Monitor{}
		}
		return e.printJSON(monitors)
	}
	tw := e.table()
	// The header goes through the same writer as the rows, so its error is
	// reported here rather than swallowed.
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tENABLED\tCONTRACTS\tCHANNELS\tLAST MATCHED"); err != nil {
		return err
	}
	for _, m := range monitors {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n",
			m.ID, m.Name, yesNo(m.Enabled), len(m.ContractIDs), joinIDs(m.ChannelIDs), formatOptionalTime(m.LastMatchedAt))
	}
	return tw.Flush()
}

func (e *cliEnv) printMonitor(m *apiclient.Monitor) error {
	if e.json {
		return e.printJSON(m)
	}
	fmt.Fprintf(e.out, "id:           %d\n", m.ID)
	fmt.Fprintf(e.out, "name:         %s\n", m.Name)
	fmt.Fprintf(e.out, "enabled:      %s\n", yesNo(m.Enabled))
	fmt.Fprintf(e.out, "contracts:    %s\n", strings.Join(m.ContractIDs, ", "))
	fmt.Fprintf(e.out, "channels:     %s\n", joinIDs(m.ChannelIDs))
	fmt.Fprintf(e.out, "created:      %s\n", formatTime(m.CreatedAt))
	fmt.Fprintf(e.out, "last matched: %s\n", formatOptionalTime(m.LastMatchedAt))
	return nil
}
