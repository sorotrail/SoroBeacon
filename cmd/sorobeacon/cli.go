// CLI subcommands. The binary runs the monitoring service when it is invoked
// with no arguments and acts as a client for a running instance when it is
// given any, so one artifact serves both the host that runs SoroBeacon and a
// CI job that scripts a change against it.
//
// The commands talk to the instance over its HTTP API through
// internal/apiclient. There is no second path to the database, so the same
// validation, authentication and authorization apply as for curl or the
// dashboard, and a CLI change never has to be mirrored in a store method.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sorotrail/sorobeacon/internal/apiclient"
)

// errHelp tells runCLI that a command printed its own usage, which is a
// successful invocation rather than a failure.
var errHelp = errors.New("help requested")

var cliUsage = `sorobeacon — monitor and manage a running SoroBeacon instance.

With no arguments the binary runs the monitoring service. With a command it
talks to a running instance over its HTTP API instead, so a deployment can be
scripted from a shell or a CI job.

Usage:
  sorobeacon                                run the monitoring service
  sorobeacon <command> [arguments] [flags]  manage a running instance
  sorobeacon help                           show this help

Commands:
  monitor list|get|create|delete|enable|disable   monitors
  rule    list|add|delete                         rules of a monitor
  channel list|create|delete|test                 notification channels

Global flags (accepted before or after the command):
  --url URL       instance base URL (default $SOROBEACON_URL, then ` +
	apiclient.DefaultBaseURL + `)
  --token TOKEN   bearer token, for an instance with API_TOKEN set
                  (default $SOROBEACON_TOKEN)
  --json          machine-readable JSON instead of the default table
  -h, --help      show this help

Run 'sorobeacon monitor', 'sorobeacon rule' or 'sorobeacon channel' for a
command group's own help.
`

const monitorUsage = `sorobeacon monitor — list and manage monitors.

Usage:
  sorobeacon monitor list [--enabled | --disabled] [--q TEXT] [--limit N]
  sorobeacon monitor get ID
  sorobeacon monitor create --name NAME --contract CONTRACT_ID
                           [--contract CONTRACT_ID ...] [--channel ID ...]
                           [--disabled]
  sorobeacon monitor delete ID
  sorobeacon monitor enable ID
  sorobeacon monitor disable ID

Monitors watch one or more contracts and alert through their channels.
`

const ruleUsage = `sorobeacon rule — list and manage a monitor's rules.

Usage:
  sorobeacon rule list MONITOR_ID
  sorobeacon rule add MONITOR_ID --type TYPE [--param KEY=VALUE ...]
                                [--params JSON] [--disabled]
  sorobeacon rule delete MONITOR_ID RULE_ID

Rule types: event_emitted, value_threshold, token_event, frequency_threshold,
address_watchlist.
--param values are typed by their JSON spelling, so count=50 is a number and
window=5m is a string; quote a value that must stay a string. --params takes
a whole JSON object and is the way to express nested params, for example:
  --params '{"event_name":"transfer","topic_equals":{"1":"GDW6...SENDER"}}'
`

const channelUsage = `sorobeacon channel — list and manage notification channels.

Usage:
  sorobeacon channel list [--type TYPE] [--enabled | --disabled] [--limit N]
  sorobeacon channel create --name NAME --type TYPE
                           [--config KEY=VALUE ...] [--config-json JSON]
                           [--disabled]
  sorobeacon channel delete ID
  sorobeacon channel test ID

Types: discord, slack, telegram, matrix, pagerduty, email, webhook.
--config values are typed by their JSON spelling, so port=587 is a number,
enabled=true a boolean and to=["ops@example.com"] an array. Config holds
secrets (webhook URLs, bot tokens, SMTP credentials): it is sent to the
instance and never read back or printed.
`

// globalOptions are the flags that apply to every command. They are split out
// of the argument list before the command's own flags are parsed, so they can
// appear on either side of the command.
type globalOptions struct {
	url   string
	token string
	json  bool
	help  bool
}

// cliEnv is what a command needs: a client pointed at one instance and the
// writer its output goes to. stdout is a parameter rather than os.Stdout so
// tests drive the same code path the binary does.
type cliEnv struct {
	client *apiclient.Client
	out    io.Writer
	json   bool
}

// runCLI executes one CLI invocation. Errors are returned rather than printed
// so main controls the exit code and the error format.
func runCLI(ctx context.Context, args []string, out io.Writer) error {
	globals, rest, err := splitGlobalFlags(args)
	if err != nil {
		return err
	}
	if globals.help {
		fmt.Fprint(out, cliUsage)
		return nil
	}

	// The instance to talk to: an explicit flag wins, then the environment
	// (how a CI job or an operator usually sets it), then the default that
	// matches the docker-compose quickstart.
	baseURL := firstNonEmpty(globals.url, os.Getenv("SOROBEACON_URL"), apiclient.DefaultBaseURL)
	token := firstNonEmpty(globals.token, os.Getenv("SOROBEACON_TOKEN"))
	env := &cliEnv{
		client: apiclient.New(baseURL, apiclient.WithToken(token)),
		out:    out,
		json:   globals.json,
	}

	if len(rest) == 0 {
		fmt.Fprint(out, cliUsage)
		return nil
	}

	switch rest[0] {
	case "help":
		fmt.Fprint(out, cliUsage)
		return nil
	case "monitor":
		err = runMonitor(ctx, env, rest[1:])
	case "rule":
		err = runRule(ctx, env, rest[1:])
	case "channel":
		err = runChannel(ctx, env, rest[1:])
	default:
		return fmt.Errorf("unknown command %q; run 'sorobeacon help'", rest[0])
	}
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}

// splitGlobalFlags pulls the global flags out of args wherever they appear
// and returns everything else in order. The flag package stops at the first
// positional argument, so parsing globals separately is what lets both
// "sorobeacon --url X monitor list" and "sorobeacon monitor list --url X"
// do the same thing.
func splitGlobalFlags(args []string) (globalOptions, []string, error) {
	var globals globalOptions
	var rest []string
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		switch name {
		case "--url", "-url", "--token", "-token":
			if !hasValue {
				if i+1 >= len(args) {
					return globals, nil, fmt.Errorf("flag %s needs a value", name)
				}
				i++
				value = args[i]
			}
			if name == "--url" || name == "-url" {
				globals.url = value
			} else {
				globals.token = value
			}
		case "--json", "-json":
			parsed, err := boolFlagValue(name, value, hasValue)
			if err != nil {
				return globals, nil, err
			}
			globals.json = parsed
		case "--help", "-help", "-h":
			// Only before the command: `sorobeacon --help` asks for the
			// top-level help, while `sorobeacon monitor list -h` belongs to
			// the command and should print that command's usage.
			if len(rest) == 0 {
				globals.help = true
				break
			}
			rest = append(rest, args[i])
		default:
			rest = append(rest, args[i])
		}
	}
	return globals, rest, nil
}

// boolFlagValue accepts --json and --json=true/false, which is what a script
// written against a boolean flag expects.
func boolFlagValue(name, value string, hasValue bool) (bool, error) {
	if !hasValue {
		return true, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("flag %s expects true or false, got %q", name, value)
	}
	return parsed, nil
}

// newFlagSet returns a flag set that keeps quiet on parse errors: the error
// is returned to main, which prints it once, so a bad flag does not produce
// the message twice.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args and turns -h/--help into "print this command's
// usage" instead of a failure, since asking for help is not an error.
func parseFlags(fs *flag.FlagSet, args []string, usage string, out io.Writer) error {
	if err := fs.Parse(permuteArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return errHelp
		}
		return err
	}
	return nil
}

// boolFlag is the interface the flag package uses to mark a flag that takes
// no value (flag.Bool and flag.BoolVar's Value).
type boolFlag interface {
	IsBoolFlag() bool
}

// permuteArgs moves a command's flags ahead of its positional arguments.
// The flag package stops parsing at the first positional argument, but
// `rule add 5 --type X` reads far more naturally than `rule add --type X 5`,
// so the natural order has to be accepted. Unknown flags are left for the
// flag set to reject rather than guessed at.
func permuteArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if hasValue {
			continue // --flag=value carries its own value
		}
		parsed := fs.Lookup(name)
		if parsed == nil {
			continue // unknown flag: let the flag set report it
		}
		if bf, ok := parsed.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// noExtraArgs rejects trailing positional arguments. They are almost always a
// typo (a flag that lost its dash) rather than intent, and silently ignoring
// them would drop the value the caller meant to set.
func noExtraArgs(fs *flag.FlagSet) error {
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// parseID reads a positive id from a command argument, so a typo is a clear
// message here rather than a round trip to the API.
func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id %q", s)
	}
	return id, nil
}

// buildJSONObject assembles a JSON object from repeatable KEY=VALUE flags, or
// from a raw JSON object when the shape is nested (rule topic_equals, an
// email to list). Values are typed by their JSON spelling, so --param
// count=50 is a number and --param window=5m a string.
func buildJSONObject(flagName string, kv []string, raw string) (json.RawMessage, error) {
	if raw != "" && len(kv) > 0 {
		return nil, fmt.Errorf("use %s or %s, not both", flagName, rawFlagName(flagName))
	}
	if raw != "" {
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
			return nil, fmt.Errorf("%s must be a JSON object", rawFlagName(flagName))
		}
		return json.RawMessage(raw), nil
	}
	if len(kv) == 0 {
		return json.RawMessage(`{}`), nil
	}
	fields := make(map[string]any, len(kv))
	for _, pair := range kv {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%s %q must be key=value", flagName, pair)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("%s %s given more than once", flagName, key)
		}
		fields[key] = parseScalar(value)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", flagName, err)
	}
	return encoded, nil
}

// rawFlagName names the whole-object sibling of a key=value flag, matching
// the pair the usage text documents (--param/--params, --config/--config-json).
func rawFlagName(flagName string) string {
	if flagName == "--config" {
		return "--config-json"
	}
	return "--params"
}

// parseScalar turns a flag value into the JSON type it looks like: 50 becomes
// a number, true a boolean, ["a"] an array, and anything else (5m, a webhook
// URL) stays a string. One flag can then express both {"count":50} and
// {"window":"5m"} without a per-field schema.
func parseScalar(value string) any {
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err == nil {
		return parsed
	}
	return value
}

// printJSON writes the machine-readable form: one indented JSON document per
// command, which pipes into jq without a separate formatter. HTML escaping is
// off so contract ids and URLs survive verbatim.
func (e *cliEnv) printJSON(v any) error {
	enc := json.NewEncoder(e.out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// table returns a tabwriter over the command's output for the human-readable
// form. Callers flush it.
func (e *cliEnv) table() *tabwriter.Writer {
	return tabwriter.NewWriter(e.out, 0, 2, 2, ' ', 0)
}

// printDeleted reports a successful DELETE. The API answers 204 with no body,
// so the CLI synthesises a confirmation in whichever format was asked for.
func (e *cliEnv) printDeleted(resource string, id int64) error {
	if e.json {
		return e.printJSON(map[string]any{"deleted": true, "id": id, "resource": resource})
	}
	fmt.Fprintf(e.out, "deleted %s %d\n", resource, id)
	return nil
}

// reportCLIError writes a failure the way a CLI should: one line carrying the
// server's message when the API sent an error envelope, plus its field
// details when the request failed validation. A 404 or a rejected channel
// config is a normal outcome, so there is no stack trace and no exit-status
// noise. When the request never reached the API (or the instance was not a
// SoroBeacon one) the message still names what was attempted.
func reportCLIError(w io.Writer, err error) {
	fmt.Fprintf(w, "sorobeacon: %s\n", err)
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return
	}
	for _, detail := range apiErr.Details {
		fmt.Fprintf(w, "sorobeacon:   %s: %s\n", detail.Field, detail.Reason)
	}
	if apiErr.RequestID != "" {
		fmt.Fprintf(w, "sorobeacon:   request id %s\n", apiErr.RequestID)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolPtr(v bool) *bool { return &v }

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// formatTime renders a timestamp for the human-readable output. The JSON form
// keeps RFC 3339 as well, so both stay sortable and unambiguous.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return formatTime(*t)
}

// joinIDs renders an id list for a table cell; "-" reads better than an empty
// column.
func joinIDs(ids []int64) string {
	if len(ids) == 0 {
		return "-"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

// stringList is a repeatable string flag: --contract A --contract B.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// int64List is the same for id-valued flags: --channel 1 --channel 2.
type int64List []int64

func (l *int64List) String() string {
	parts := make([]string, len(*l))
	for i, id := range *l {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func (l *int64List) Set(value string) error {
	id, err := parseID(value)
	if err != nil {
		return err
	}
	*l = append(*l, id)
	return nil
}
