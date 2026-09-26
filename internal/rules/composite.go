package rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sorotrail/sorobeacon/internal/stellar"
)

// TypeComposite is the rule type that combines child rules with a boolean
// operator.
const TypeComposite = "composite"

// Composite operators. "not" is a unary operator and takes exactly one child;
// "and" and "or" are variadic and require at least one.
const (
	opAnd = "and"
	opOr  = "or"
	opNot = "not"
)

// maxCompositeDepth caps how deeply composite rules may nest, counting the
// outermost composite as level 1. A composite already at this depth cannot
// contain another composite child, so validation and evaluation stay bounded
// on parameters that arrived from a user. Five levels is well past any
// realistic rule and far short of a stack-exhausting depth.
const maxCompositeDepth = 5

// compositeParams configure the composite rule.
//
//	{
//	  "op": "and",                       // required: and | or | not
//	  "rules": [                          // required: at least one, exactly
//	    {"type": "token_event",           //   one for "not"
//	     "params": {"event": "transfer", "min_amount": "1000000"}},
//	    {"type": "event_emitted",
//	     "params": {"event_name": "transfer"}}
//	  ]
//	}
//
// The children are ordinary rule params and are validated through the
// registry, so a composite accepts every rule type the registry knows —
// including another composite, up to maxCompositeDepth.
type compositeParams struct {
	Op    string           `json:"op"`
	Rules []compositeChild `json:"rules"`
}

// compositeChild is one child rule: a registered type name plus that type's
// own params blob, exactly as a top-level rule would carry them.
type compositeChild struct {
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// Composite evaluates child rules and combines their results with and/or/not.
//
// The RuleEvaluator interface carries no registry, so a composite resolves its
// children through the *Registry it was built with. NewRegistry constructs it
// with a pointer to itself, which means a child type registered later — as
// long as it is registered on the same registry — is visible here without
// rewiring.
//
// Evaluation is short-circuiting: "and" stops at the first false child and
// "or" at the first true one, so a composite never evaluates a child whose
// result cannot change the outcome.
type Composite struct {
	registry *Registry
}

// NewComposite returns a composite evaluator that resolves child rules through
// reg. The registry must be non-nil; NewRegistry passes itself.
func NewComposite(reg *Registry) *Composite {
	return &Composite{registry: reg}
}

// Validate checks the operator, the child count, and every child rule
// recursively. Child params are validated through the registry, so an unknown
// child type and a malformed grandchild are both create-time errors; reported
// field paths are rooted at this rule (for example rules[1].params), which
// names the exact offending child.
func (c *Composite) Validate(params json.RawMessage) error {
	details := c.validate(params, "", 1)
	if len(details) > 0 {
		return details
	}
	return nil
}

// validate collects every problem in one params blob. prefix is the field path
// back to this blob ("" at the root, rules[i].params for a child); depth is the
// composite nesting level of the blob being validated, with the root at 1.
func (c *Composite) validate(params json.RawMessage, prefix string, depth int) FieldErrors {
	var details FieldErrors
	p, err := parseComposite(params)
	if err != nil {
		return append(details, FieldError{Field: prefix, Reason: err.Error()})
	}

	switch p.Op {
	case opAnd, opOr:
		if len(p.Rules) == 0 {
			details = append(details, FieldError{
				Field:  fieldPath(prefix, "rules"),
				Reason: fmt.Sprintf("composite: op %q requires at least one child rule", p.Op),
			})
		}
	case opNot:
		if len(p.Rules) != 1 {
			details = append(details, FieldError{
				Field:  fieldPath(prefix, "rules"),
				Reason: `composite: op "not" requires exactly one child rule`,
			})
		}
	case "":
		details = append(details, FieldError{
			Field:  fieldPath(prefix, "op"),
			Reason: "composite: op is required (and|or|not)",
		})
	default:
		details = append(details, FieldError{
			Field:  fieldPath(prefix, "op"),
			Reason: fmt.Sprintf("composite: unknown op %q (want and|or|not)", p.Op),
		})
	}

	for i, child := range p.Rules {
		base := fieldPath(prefix, fmt.Sprintf("rules[%d]", i))
		paramsPath := fieldPath(base, "params")
		if child.Type == "" {
			details = append(details, FieldError{
				Field:  fieldPath(base, "type"),
				Reason: "composite: child rule type is required",
			})
			continue
		}
		if child.Type == TypeComposite {
			if depth >= maxCompositeDepth {
				details = append(details, FieldError{
					Field:  paramsPath,
					Reason: fmt.Sprintf("composite: rules may not nest more than %d levels deep", maxCompositeDepth),
				})
				continue
			}
			// A nested composite is validated here rather than through the
			// registry so the depth is carried across levels; registry.Validate
			// would restart the count at 1 and the limit would never be hit.
			// Its cooldown is checked explicitly because bypassing the registry
			// also bypasses the cooldown check there.
			if _, err := ParseCooldown(child.Params); err != nil {
				details = append(details, prefixFields(paramsPath, err)...)
			}
			details = append(details, c.validate(child.Params, paramsPath, depth+1)...)
			continue
		}
		// An unknown child type is a validation error here, not a panic later:
		// Registry.Validate reports it before the rule can be stored.
		if err := c.registry.Validate(child.Type, child.Params); err != nil {
			details = append(details, prefixFields(paramsPath, err)...)
		}
	}
	return details
}

// Evaluate combines the child rules' results. It short-circuits: "and" returns
// false at the first failing child, "or" returns true at the first matching
// one, and neither evaluates the children after that point.
func (c *Composite) Evaluate(ctx context.Context, ev *stellar.DecodedEvent, params json.RawMessage) (bool, error) {
	p, err := parseComposite(params)
	if err != nil {
		return false, err
	}
	switch p.Op {
	case opAnd:
		for _, child := range p.Rules {
			ok, err := c.evalChild(ctx, ev, child)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	case opOr:
		for _, child := range p.Rules {
			ok, err := c.evalChild(ctx, ev, child)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case opNot:
		if len(p.Rules) != 1 {
			return false, fmt.Errorf(`composite: op "not" requires exactly one child rule`)
		}
		ok, err := c.evalChild(ctx, ev, p.Rules[0])
		if err != nil {
			return false, err
		}
		return !ok, nil
	default:
		return false, fmt.Errorf("composite: unknown op %q (want and|or|not)", p.Op)
	}
}

// evalChild resolves and runs one child rule. A child type the registry does
// not know returns an error rather than panicking; Validate is expected to
// have rejected it at create time, so reaching here means the registry changed
// under a stored rule.
func (c *Composite) evalChild(ctx context.Context, ev *stellar.DecodedEvent, child compositeChild) (bool, error) {
	ok, err := c.registry.Evaluate(ctx, child.Type, ev, child.Params)
	if err != nil {
		return false, fmt.Errorf("composite: child %q: %w", child.Type, err)
	}
	return ok, nil
}

// fieldPath joins a field path with one more segment, keeping the root path
// bare (fieldPath("", "op") is "op") so a top-level error reads like every
// other evaluator's.
func fieldPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// prefixFields rewrites an error from a child's Validate so its field path is
// rooted at the child (params prefix) inside this composite. FieldError and
// FieldErrors are both handled; anything else is reported at the prefix.
func prefixFields(prefix string, err error) FieldErrors {
	switch e := err.(type) {
	case FieldErrors:
		out := make(FieldErrors, 0, len(e))
		for _, d := range e {
			out = append(out, FieldError{Field: joinPath(prefix, d.Field), Reason: d.Reason})
		}
		return out
	case FieldError:
		return FieldErrors{{Field: joinPath(prefix, e.Field), Reason: e.Reason}}
	default:
		return FieldErrors{{Field: prefix, Reason: err.Error()}}
	}
}

// joinPath appends a child-relative field path to its prefix, leaving the
// prefix alone when the child error names no field.
func joinPath(prefix, field string) string {
	if field == "" {
		return prefix
	}
	return prefix + "." + field
}

func parseComposite(params json.RawMessage) (compositeParams, error) {
	var p compositeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return p, fmt.Errorf("composite: invalid params: %w", err)
	}
	return p, nil
}
