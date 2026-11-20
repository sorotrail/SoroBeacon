# Kubernetes operator (Channel resource)

The operator lets a cluster declare SoroBeacon channels as Kubernetes objects so
a change to who gets paged is a reviewable diff. This first version covers one
resource, `Channel`, end to end. Monitors, rules and escalation policies are not
mirrored; the Terraform provider (`terraform-provider-sorobeacon`) already covers
those and is the better fit when you do not run Kubernetes.

It lives in its own Go module under `operator/` with no dependencies beyond the
standard library, and is off by default in the chart (`operator.enabled=false`).

## Resource

```yaml
apiVersion: sorobeacon.io/v1alpha1
kind: Channel
metadata:
  name: oncall-slack
spec:
  type: slack
  minSeverity: warning
  secretRefs:
    - {configKey: webhook_url, name: oncall-slack-webhook, key: url}
  deletionPolicy: Orphan
```

`spec.config` holds non-secret settings. Secrets are never inlined: each
`secretRefs` entry copies one Secret value into one config key when the operator
calls the API. The value is not written to the resource, its status, events or
logs.

## Behaviour

- **Reconcile** is idempotent and convergent. Each pass lists channels through
  the public API (so validation and workspace guards apply), creates the channel
  if missing, and updates it when something differs. A pass over an in-sync
  resource writes nothing.
- **Drift.** Name, type, enabled and minSeverity edited out of band are reverted
  and reported in `status.message`. The API never returns channel configuration,
  so configuration drift cannot be observed; configuration is re-applied when the
  resource or a referenced Secret changes (tracked by a digest in
  `status.appliedHash`).
- **Coexistence with imperative use.** The operator manages only channels it
  created, or ones it was told to take over with `spec.adopt: true`. A name
  collision with a hand-made channel is reported as an error, not overwritten.
- **Deletion.** A finalizer is added before anything is created. Deleting the
  resource removes the finalizer and, by default (`Orphan`), leaves the channel
  and its delivery history in SoroBeacon. `deletionPolicy: Delete` removes that
  one channel. If the API cannot confirm, the finalizer stays and the resource
  remains with `phase: Deleting`. Nothing here deletes alerts, monitors or a
  workspace.
- **Status.** `phase` is `Applied`, `Error` or `Deleting`, with a `message`
  (never including secret values or response bodies), `channelID` and
  `observedGeneration`.
- **Authentication.** The operator uses a scoped API token (channels read and
  write, one workspace), from an existing Secret named by
  `operator.tokenSecret`. Its Kubernetes permissions are Channel resources
  (get, list, patch) and `get` on Secrets in the release namespace only.

## Install

```sh
helm install sorobeacon deploy/helm/sorobeacon \
  --set operator.enabled=true --set operator.tokenSecret.name=sorobeacon-operator-token
```

## Limitations

- Polling (default 30s), not watches: a change is applied on the next pass.
- No leader election; run one replica.
- Not controller-runtime: this is a small purpose-built reconciler, chosen to
  keep the dependency tree empty. A move to controller-runtime would bring
  watches and leader election.
- Channel resources and the Secrets they reference must live in the release
  namespace.
- Only `Channel`. Monitors, rules and escalation policies are not supported.
- The operator image is built from `operator/` (`go build ./cmd/operator`); this
  change does not publish one.
