# Extending SoroBeacon

SoroBeacon's core is deliberately small; features are meant to arrive as implementations of five interfaces:

| To add… | Implement | Register / wire in |
| --- | --- | --- |
| a notification channel | `notify.Notifier` | `DefaultFactory` in `internal/notify/notify.go` |
| a rule type | `rules.RuleEvaluator` | `NewRegistry` in `internal/rules/rules.go` |
| a different event source | `stellar.Client` | `cmd/sorobeacon/main.go` |
| smarter event decoding | `stellar.Decoder` | `cmd/sorobeacon/main.go` |
| specs from another source | `stellar.SpecSource` | `stellar.NewSpecDecoder` wiring in `cmd/sorobeacon/main.go` |
| another database | `store.Store` (or a sub-interface) | `cmd/sorobeacon/main.go` |

## Adding a channel

```go
// internal/notify/matrix.go
type matrixConfig struct {
	HomeserverURL string `json:"homeserver_url"`
	AccessToken   string `json:"access_token"`
	RoomID        string `json:"room_id"`
}

func NewMatrix(config json.RawMessage) (Notifier, error) {
	var cfg matrixConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("matrix: invalid config: %w", err)
	}
	// Validate here: the API calls this constructor to reject bad
	// channels at create time.
	...
}

func (m *Matrix) Send(ctx context.Context, a Alert) error {
	msg, err := RenderText(a) // the shared plain-text alert template
	...
}
```

Register it in `DefaultFactory`:

```go
f.Register("matrix", NewMatrix)
```

Rules of the road:

* **Never** put secrets in error messages — errors become delivery `response_snippet`s and log lines.
* Non-2xx / failure = return an error; the dispatcher owns retries and recording.
* Ship a unit test (see `webhook_test.go` for the `httptest` pattern).

## Adding a rule type

```go
// internal/rules/absence.go
type Absence struct{}

func (Absence) Validate(params json.RawMessage) error { ... }

func (Absence) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) { ... }
```

Register it in `NewRegistry`:

```go
r.Register("absence", Absence{})
```

Evaluators must be stateless and concurrency-safe, with one exception:
`frequency_threshold` keeps a rolling window per rule, keyed by the rule id the
poller puts in the context (`rules.WithRuleID`) and rebuilt from the `alerts`
table after a restart. An evaluator that genuinely needs state should follow the
same pattern. Decoded events use a small value vocabulary (`nil`, `bool`, `string`, `*big.Int`, `[]byte`, `[]any`, `map[string]any`); build on the helpers in `internal/stellar`:

* `Canon(v)` — canonical string rendering for equality comparisons
* `ToBigFloat(v)` — arbitrary-precision numeric coercion
* `Lookup(v, "a.b.0")` — dot-path resolution into maps/slices

## Named event fields

`stellar.SpecDecoder` wraps any `stellar.Decoder` and fills
`DecodedEvent.Fields` from the contract's SEP-0048 spec, so rule authors can
write `value_path: "amount"` instead of `value_path: "..."` against topic
positions. It is additive: `Topics` and `Value` keep their positional
decoding, and a contract with no spec (or a fetch that fails) decodes exactly
as the wrapped decoder would. Specs are fetched through `stellar.SpecSource`
(the RPC-backed default is `stellar.NewRPCSpecSource`) and cached per
contract, including negative results. Swap in a different `SpecSource` if
specs live somewhere else, such as an indexer or a local cache.

## Wanted (open by design)

* Rule types: absence-of-event ("no heartbeat for N minutes")
* Channels: ntfy.sh
* A richer dashboard
