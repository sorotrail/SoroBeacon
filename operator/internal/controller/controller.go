package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// ErrNotFound is what a Beacon returns for a channel that does not exist.
var ErrNotFound = errors.New("not found")

// Reconciler drives Channel resources toward their declared state.
//
// Conflict policy with imperative use: the operator owns a channel only if it
// created it or was told to adopt it (spec.adopt). It never touches any other
// channel, so API and UI users keep their own channels. For an owned channel
// the declared state wins: an out-of-band edit to name, type, enabled or
// minSeverity is reverted on the next pass and reported in status.
//
// Deletion policy: a finalizer guards every resource. Deleting the manifest
// removes the finalizer and, by default (Orphan), leaves the channel and its
// delivery history in SoroBeacon. Only deletionPolicy: Delete removes the
// channel itself, and nothing here ever deletes alerts, monitors or a
// workspace. If the API is unreachable when a Delete-policy resource is
// removed, the finalizer stays and the resource remains until the API answers.
type Reconciler struct {
	Kube   Kube
	Beacon Beacon
	Log    *slog.Logger
}

// ReconcileAll lists every Channel and reconciles each independently: one bad
// resource never stops the others.
func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	list, err := r.Kube.ListChannels(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range list {
		r.Reconcile(ctx, ch)
	}
	return nil
}

// Reconcile handles one resource and records the outcome in its status.
func (r *Reconciler) Reconcile(ctx context.Context, ch Channel) {
	st := ch.Status
	if ch.Metadata.DeletionTimestamp != "" {
		r.finalize(ctx, ch)
		return
	}
	if !ch.hasFinalizer() {
		// Before anything is created, so a delete cannot slip past.
		if err := r.Kube.SetFinalizers(ctx, ch.Metadata.Namespace, ch.Metadata.Name, append(slices.Clone(ch.Metadata.Finalizers), Finalizer)); err != nil {
			r.log().Warn("set finalizer failed", "namespace", ch.Metadata.Namespace, "name", ch.Metadata.Name)
			return
		}
	}
	next := r.apply(ctx, ch)
	next.ObservedGeneration = ch.Metadata.Generation
	if next != st {
		if err := r.Kube.UpdateStatus(ctx, ch.Metadata.Namespace, ch.Metadata.Name, next); err != nil {
			r.log().Warn("status update failed", "namespace", ch.Metadata.Namespace, "name", ch.Metadata.Name)
		}
	}
}

func (r *Reconciler) fail(st ChannelStatus, msg string) ChannelStatus {
	st.Phase, st.Message = PhaseError, msg
	return st
}

// apply makes the remote channel match the spec and returns the new status.
func (r *Reconciler) apply(ctx context.Context, ch Channel) ChannelStatus {
	st := ch.Status
	if ch.Spec.Type == "" {
		return r.fail(st, "spec.type is required")
	}
	if p := ch.Spec.DeletionPolicy; p != "" && p != DeletionOrphan && p != DeletionDelete {
		return r.fail(st, "spec.deletionPolicy must be Orphan or Delete")
	}
	body, err := r.desired(ctx, ch)
	if err != nil {
		return r.fail(st, err.Error())
	}
	hash := hashBody(body)

	remote, err := r.Beacon.List(ctx)
	if err != nil {
		return r.fail(st, "SoroBeacon API unreachable or refused the list request; will retry")
	}
	var current *RemoteChannel
	for i := range remote {
		if st.ChannelID != 0 && remote[i].ID == st.ChannelID {
			current = &remote[i]
		}
	}
	if current == nil {
		for i := range remote {
			if remote[i].Name != body.Name {
				continue
			}
			if !ch.Spec.Adopt {
				return r.fail(st, fmt.Sprintf("a channel named %q already exists and is not managed by this resource; set spec.adopt to take it over", body.Name))
			}
			current = &remote[i]
			st.ChannelID = current.ID
			st.AppliedHash = "" // force a write: adopted config is unknown
		}
	}
	if current == nil {
		id, err := r.Beacon.Create(ctx, body)
		if err != nil {
			return r.fail(st, "create rejected: "+safeErr(err))
		}
		st.ChannelID, st.AppliedHash, st.Phase, st.Message = id, hash, PhaseApplied, "created"
		return st
	}
	drifted := current.Name != body.Name || current.Type != body.Type || current.Enabled != body.Enabled || current.MinSeverity != body.MinSeverity
	if drifted || st.AppliedHash != hash {
		if err := r.Beacon.Update(ctx, current.ID, body); err != nil {
			return r.fail(st, "update rejected: "+safeErr(err))
		}
		st.AppliedHash, st.Phase = hash, PhaseApplied
		st.Message = "updated"
		if drifted {
			st.Message = "restored declared state after out-of-band change"
		}
		return st
	}
	st.Phase, st.Message = PhaseApplied, "in sync"
	return st
}

// desired builds the API body, reading each referenced Secret value.
func (r *Reconciler) desired(ctx context.Context, ch Channel) (ChannelBody, error) {
	cfg := map[string]any{}
	for k, v := range ch.Spec.Config {
		cfg[k] = v
	}
	cache := map[string]map[string][]byte{}
	for _, ref := range ch.Spec.SecretRefs {
		if ref.ConfigKey == "" || ref.Name == "" || ref.Key == "" {
			return ChannelBody{}, errors.New("each secretRef needs configKey, name and key")
		}
		data, ok := cache[ref.Name]
		if !ok {
			var err error
			data, err = r.Kube.GetSecret(ctx, ch.Metadata.Namespace, ref.Name)
			if err != nil {
				return ChannelBody{}, fmt.Errorf("secret %q could not be read", ref.Name)
			}
			cache[ref.Name] = data
		}
		v, ok := data[ref.Key]
		if !ok {
			return ChannelBody{}, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
		}
		cfg[ref.ConfigKey] = string(v)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return ChannelBody{}, errors.New("spec.config is not valid JSON")
	}
	return ChannelBody{Name: ch.channelName(), Type: ch.Spec.Type, Enabled: ch.enabled(), MinSeverity: ch.Spec.MinSeverity, Config: raw}, nil
}

func hashBody(b ChannelBody) string {
	raw, _ := json.Marshal(b) // map keys inside Config are marshalled sorted
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// safeErr keeps only what the API client already vetted (a status), never a
// response body or request detail.
func safeErr(err error) string {
	var se interface{ StatusCode() int }
	if errors.As(err, &se) {
		return fmt.Sprintf("HTTP %d", se.StatusCode())
	}
	return "request failed"
}

func (r *Reconciler) finalize(ctx context.Context, ch Channel) {
	if !ch.hasFinalizer() {
		return
	}
	st := ch.Status
	if ch.Spec.DeletionPolicy == DeletionDelete && st.ChannelID != 0 {
		if err := r.Beacon.Delete(ctx, st.ChannelID); err != nil && !errors.Is(err, ErrNotFound) {
			st.Phase, st.Message = PhaseDeleting, "delete blocked, SoroBeacon API did not confirm: "+safeErr(err)
			if st != ch.Status {
				_ = r.Kube.UpdateStatus(ctx, ch.Metadata.Namespace, ch.Metadata.Name, st)
			}
			return // finalizer stays: nothing is forgotten silently
		}
	}
	rest := slices.DeleteFunc(slices.Clone(ch.Metadata.Finalizers), func(f string) bool { return f == Finalizer })
	if err := r.Kube.SetFinalizers(ctx, ch.Metadata.Namespace, ch.Metadata.Name, rest); err != nil {
		r.log().Warn("remove finalizer failed", "namespace", ch.Metadata.Namespace, "name", ch.Metadata.Name)
	}
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
