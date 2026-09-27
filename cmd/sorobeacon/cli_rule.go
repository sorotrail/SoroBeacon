package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// runRule dispatches the rule command group. Rules belong to a monitor, so
// every command takes a monitor id first.
func runRule(ctx context.Context, env *cliEnv, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(env.out, ruleUsage)
		return errHelp
	}
	switch args[0] {
	case "list":
		return ruleList(ctx, env, args[1:])
	case "add":
		return ruleAdd(ctx, env, args[1:])
	case "delete":
		return ruleDelete(ctx, env, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(env.out, ruleUsage)
		return errHelp
	default:
		return fmt.Errorf("unknown rule command %q; run 'sorobeacon rule' for usage", args[0])
	}
}

func ruleList(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("rule list")
	if err := parseFlags(fs, args, ruleUsage, env.out); err != nil {
		return err
	}
	monitorID, err := singleIDArg(fs, "sorobeacon rule list MONITOR_ID")
	if err != nil {
		return err
	}
	rules, err := env.client.ListRules(ctx, monitorID)
	if err != nil {
		return err
	}
	if env.json {
		if rules == nil {
			rules = []apiclient.Rule{}
		}
		return env.printJSON(rules)
	}
	tw := env.table()
	if _, err := fmt.Fprintln(tw, "ID\tTYPE\tENABLED\tPARAMS"); err != nil {
		return err
	}
	for _, r := range rules {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", r.ID, r.Type, yesNo(r.Enabled), paramsText(r.Params))
	}
	return tw.Flush()
}

func ruleAdd(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("rule add")
	ruleType := fs.String("type", "", "rule type (required)")
	var params stringList
	fs.Var(&params, "param", "rule parameter as key=value (repeatable)")
	paramsJSON := fs.String("params", "", "rule parameters as a JSON object, for nested values")
	disabled := fs.Bool("disabled", false, "add the rule disabled")
	if err := parseFlags(fs, args, ruleUsage, env.out); err != nil {
		return err
	}
	monitorID, err := singleIDArg(fs, "sorobeacon rule add MONITOR_ID --type TYPE")
	if err != nil {
		return err
	}
	if *ruleType == "" {
		return errors.New("--type is required")
	}
	built, err := buildJSONObject("--param", params, *paramsJSON)
	if err != nil {
		return err
	}

	in := apiclient.RuleCreate{Type: *ruleType, Params: built}
	if *disabled {
		in.Enabled = boolPtr(false)
	}
	rule, err := env.client.CreateRule(ctx, monitorID, in)
	if err != nil {
		return err
	}
	return env.printRule(rule)
}

func ruleDelete(ctx context.Context, env *cliEnv, args []string) error {
	fs := newFlagSet("rule delete")
	if err := parseFlags(fs, args, ruleUsage, env.out); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: sorobeacon rule delete MONITOR_ID RULE_ID")
	}
	monitorID, err := parseID(fs.Arg(0))
	if err != nil {
		return err
	}
	ruleID, err := parseID(fs.Arg(1))
	if err != nil {
		return err
	}
	if err := env.client.DeleteRule(ctx, monitorID, ruleID); err != nil {
		return err
	}
	return env.printDeleted("rule", ruleID)
}

func (e *cliEnv) printRule(rule *apiclient.Rule) error {
	if e.json {
		return e.printJSON(rule)
	}
	fmt.Fprintf(e.out, "id:         %d\n", rule.ID)
	fmt.Fprintf(e.out, "monitor:    %d\n", rule.MonitorID)
	fmt.Fprintf(e.out, "type:       %s\n", rule.Type)
	fmt.Fprintf(e.out, "enabled:    %s\n", yesNo(rule.Enabled))
	fmt.Fprintf(e.out, "params:     %s\n", paramsText(rule.Params))
	return nil
}

// paramsText renders a rule's params for the human-readable output. Params
// are rule configuration; unlike a channel's config they hold no secrets.
func paramsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
