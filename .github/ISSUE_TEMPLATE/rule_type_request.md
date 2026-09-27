---
name: Rule type request
about: Request a monitoring rule type (or volunteer to add one)
labels: enhancement, rule-type
---

**The monitoring question**

In plain language: what should SoroBeacon watch for? ("page me when
this contract is silent for 10 minutes", "alert if more than 50
transfers fire in a minute").

**An event it should match**

A concrete example of a chain event (or absence) that should fire
the rule.

**An event it should not match**

A nearby example that must stay quiet, so the matching semantics
are unambiguous.

**Parameters you would configure**

The knobs a user would set on the rule (window, threshold, event
name, …).

**How contained the work is**

A new rule type is a `rules.RuleEvaluator` registered in
`NewRegistry` in `internal/rules`. If that is enough for you to
send a PR instead of a request, please do — CONTRIBUTING.md has
the plug-in table.
