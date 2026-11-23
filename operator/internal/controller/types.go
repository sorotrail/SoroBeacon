// Package controller reconciles Channel custom resources against the
// SoroBeacon public API.
package controller

import (
	"context"
	"encoding/json"
)

// Finalizer is set on every Channel before the operator creates anything, so
// a delete always passes through reconciliation.
const Finalizer = "sorobeacon.io/channel-protection"

// Deletion policies. Orphan is the default: removing the manifest stops
// managing the channel and leaves it, and its delivery history, in SoroBeacon.
const (
	DeletionOrphan = "Orphan"
	DeletionDelete = "Delete"
)

// Status phases.
const (
	PhaseApplied  = "Applied"
	PhaseError    = "Error"
	PhaseDeleting = "Deleting"
)

type Meta struct {
	Name              string   `json:"name"`
	Namespace         string   `json:"namespace"`
	Generation        int64    `json:"generation,omitempty"`
	DeletionTimestamp string   `json:"deletionTimestamp,omitempty"`
	Finalizers        []string `json:"finalizers,omitempty"`
}

// SecretRef pulls one value of a Kubernetes Secret into one config key. The
// value is read at reconcile time and sent to the API; it is never copied into
// the custom resource, its status, an event or a log line.
type SecretRef struct {
	ConfigKey string `json:"configKey"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

type ChannelSpec struct {
	// Name is the SoroBeacon channel name; defaults to metadata.name.
	Name        string         `json:"name,omitempty"`
	Type        string         `json:"type"`
	Enabled     *bool          `json:"enabled,omitempty"`
	MinSeverity string         `json:"minSeverity,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	SecretRefs  []SecretRef    `json:"secretRefs,omitempty"`
	// Adopt lets the operator take over an existing channel of the same name.
	// Off by default so a name collision with a hand-made channel is reported
	// rather than overwritten.
	Adopt          bool   `json:"adopt,omitempty"`
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

type ChannelStatus struct {
	Phase              string `json:"phase,omitempty"`
	Message            string `json:"message,omitempty"`
	ChannelID          int64  `json:"channelID,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// AppliedHash is a digest of the desired channel including secret values,
	// so a rotated Secret is re-applied. It is a SHA-256 and reveals nothing.
	AppliedHash string `json:"appliedHash,omitempty"`
}

type Channel struct {
	Metadata Meta          `json:"metadata"`
	Spec     ChannelSpec   `json:"spec"`
	Status   ChannelStatus `json:"status,omitempty"`
}

func (c Channel) channelName() string {
	if c.Spec.Name != "" {
		return c.Spec.Name
	}
	return c.Metadata.Name
}

func (c Channel) enabled() bool { return c.Spec.Enabled == nil || *c.Spec.Enabled }

func (c Channel) hasFinalizer() bool {
	for _, f := range c.Metadata.Finalizers {
		if f == Finalizer {
			return true
		}
	}
	return false
}

// Kube is the slice of the Kubernetes API the controller needs.
type Kube interface {
	ListChannels(ctx context.Context) ([]Channel, error)
	GetSecret(ctx context.Context, namespace, name string) (map[string][]byte, error)
	SetFinalizers(ctx context.Context, namespace, name string, finalizers []string) error
	UpdateStatus(ctx context.Context, namespace, name string, status ChannelStatus) error
}

// RemoteChannel is a channel as the SoroBeacon API reports it. The API never
// returns channel configuration, so configuration drift cannot be observed.
type RemoteChannel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Enabled     bool   `json:"enabled"`
	MinSeverity string `json:"min_severity"`
}

// ChannelBody is what is sent to create or update a channel.
type ChannelBody struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Enabled     bool            `json:"enabled"`
	MinSeverity string          `json:"min_severity"`
	Config      json.RawMessage `json:"config"`
}

// Beacon is the slice of the SoroBeacon API the controller needs.
type Beacon interface {
	List(ctx context.Context) ([]RemoteChannel, error)
	Create(ctx context.Context, b ChannelBody) (int64, error)
	Update(ctx context.Context, id int64, b ChannelBody) error
	Delete(ctx context.Context, id int64) error
}
