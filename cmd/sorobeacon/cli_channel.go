package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// runChannel dispatches the channel command group.
func runChannel(ctx context.Context, env *cliEnv, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(env.out, channelUsage)
		return errHelp
	}
	switch args[0] {
	case "list":
		return channelList(ctx, env, args[1:])
	case "create":
		return channelCreate(ctx, env, args[1:])
	case "delete":
		return channelDelete(ctx, env, args[1:])
	case "test":
		return channelTest(ctx, env, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(env.out, channelUsage)
		return errHelp
	default:
		return fmt.Errorf("unknown channel command %q; run 'sorobeacon channel' for usage", args[0])
	}
}

func channelList(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("channel list")
	channelType := fs.String("type", "", "filter by channel type")
	enabled := fs.Bool("enabled", false, "only enabled channels")
	disabled := fs.Bool("disabled", false, "only disabled channels")
	limit := fs.Int("limit", 0, "page size (server default 50, max 500)")
	if err := parseFlags(fs, args, channelUsage, env.out); err != nil {
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if *enabled && *disabled {
		return errors.New("--enabled and --disabled are mutually exclusive")
	}

	opts := apiclient.ListOptions{Type: *channelType, Limit: *limit}
	switch {
	case *enabled:
		opts.Enabled = boolPtr(true)
	case *disabled:
		opts.Enabled = boolPtr(false)
	}
	channels, err := env.client.ListChannels(ctx, opts)
	if err != nil {
		return err
	}
	return env.printChannelList(channels)
}

// channelCreate takes the config as KEY=VALUE flags (or one JSON object) and
// sends it to the instance. The config is never printed back: the API strips
// it from responses and apiclient.Channel has no field for it, so a webhook
// URL or bot token cannot reach stdout, a log line or a CI transcript.
func channelCreate(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("channel create")
	name := fs.String("name", "", "channel name (required)")
	channelType := fs.String("type", "", "channel type (required)")
	var config stringList
	fs.Var(&config, "config", "config entry as key=value (repeatable)")
	configJSON := fs.String("config-json", "", "config as a JSON object, for nested values")
	disabled := fs.Bool("disabled", false, "create the channel disabled")
	if err := parseFlags(fs, args, channelUsage, env.out); err != nil {
		return err
	}
	if err := noExtraArgs(fs); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	if *channelType == "" {
		return errors.New("--type is required")
	}
	built, err := buildJSONObject("--config", config, *configJSON)
	if err != nil {
		return err
	}

	in := apiclient.ChannelCreate{Name: *name, Type: *channelType, Config: built}
	if *disabled {
		in.Enabled = boolPtr(false)
	}
	ch, err := env.client.CreateChannel(ctx, in)
	if err != nil {
		return err
	}
	return env.printChannel(ch)
}

func channelDelete(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("channel delete")
	if err := parseFlags(fs, args, channelUsage, env.out); err != nil {
		return err
	}
	id, err := singleIDArg(fs, "sorobeacon channel delete ID")
	if err != nil {
		return err
	}
	if err := env.client.DeleteChannel(ctx, id); err != nil {
		return err
	}
	return env.printDeleted("channel", id)
}

// channelTest asks the instance to deliver a synthetic alert, which is the
// only way to prove a webhook URL or bot token actually works end to end.
// Delivery failures come back as the server's message (a 502), so the CLI
// needs no channel-specific knowledge.
func channelTest(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("channel test")
	if err := parseFlags(fs, args, channelUsage, env.out); err != nil {
		return err
	}
	id, err := singleIDArg(fs, "sorobeacon channel test ID")
	if err != nil {
		return err
	}
	if err := env.client.TestChannel(ctx, id); err != nil {
		return err
	}
	if env.json {
		return env.printJSON(map[string]any{"id": id, "status": "sent"})
	}
	fmt.Fprintf(env.out, "test alert sent through channel %d\n", id)
	return nil
}

// printChannelList prints a channel table. There is deliberately no config
// column: config holds secrets, and the API does not return it at all.
func (e *cliEnv) printChannelList(channels []apiclient.Channel) error {
	if e.json {
		if channels == nil {
			channels = []apiclient.Channel{}
		}
		return e.printJSON(channels)
	}
	tw := e.table()
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tTYPE\tENABLED\tCREATED"); err != nil {
		return err
	}
	for _, ch := range channels {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", ch.ID, ch.Name, ch.Type, yesNo(ch.Enabled), formatTime(ch.CreatedAt))
	}
	return tw.Flush()
}

func (e *cliEnv) printChannel(ch *apiclient.Channel) error {
	if e.json {
		return e.printJSON(ch)
	}
	fmt.Fprintf(e.out, "id:      %d\n", ch.ID)
	fmt.Fprintf(e.out, "name:    %s\n", ch.Name)
	fmt.Fprintf(e.out, "type:    %s\n", ch.Type)
	fmt.Fprintf(e.out, "enabled: %s\n", yesNo(ch.Enabled))
	fmt.Fprintf(e.out, "created: %s\n", formatTime(ch.CreatedAt))
	return nil
}
