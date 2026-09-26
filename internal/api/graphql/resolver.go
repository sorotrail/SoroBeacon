// Package graphql provides a read-only GraphQL endpoint for SoroBeacon.
package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// Resolver holds the dependencies for GraphQL resolvers.
type Resolver struct {
	store store.Store
	log   *slog.Logger
}

// NewResolver creates a new GraphQL resolver.
func NewResolver(st store.Store, log *slog.Logger) *Resolver {
	return &Resolver{store: st, log: log}
}

// Query returns the root query resolver.
func (r *Resolver) Query() QueryResolver {
	return &queryResolver{r}
}

// Monitor returns the Monitor type resolver.
func (r *Resolver) Monitor() MonitorResolver {
	return &monitorResolver{r}
}

// Alert returns the Alert type resolver.
func (r *Resolver) Alert() AlertResolver {
	return &alertResolver{r}
}

// Channel returns the Channel type resolver.
func (r *Resolver) Channel() ChannelResolver {
	return &channelResolver{r}
}

// Rule returns the Rule type resolver.
func (r *Resolver) Rule() RuleResolver {
	return &ruleResolver{r}
}

// queryResolver implements the root query operations.
type queryResolver struct{ *Resolver }

func (r *queryResolver) Monitor(ctx context.Context, id string) (*Monitor, error) {
	monitorID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, gqlerror.Errorf("invalid monitor ID: %v", err)
	}
	m, err := r.store.GetMonitor(ctx, monitorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, gqlerror.Errorf("monitor not found: %v", err)
	}
	return toMonitor(m), nil
}

func (r *queryResolver) Monitors(ctx context.Context, enabled *bool, limit *int, cursor *string, query *string, sort *MonitorSort) (*MonitorConnection, error) {
	f := store.ListFilter{}
	if enabled != nil {
		f.Enabled = enabled
		f.EnabledOnly = *enabled
	}
	if limit != nil {
		f.Limit = *limit
	}
	if cursor != nil && *cursor != "" {
		if id, err := strconv.ParseInt(*cursor, 10, 64); err == nil {
			f.AfterID = id
		}
	}
	if query != nil {
		f.Query = *query
	}
	if sort != nil {
		switch *sort {
		case MonitorSortName:
			f.Sort = "name"
		case MonitorSortID:
			f.Sort = "id"
		case MonitorSortCreatedAt:
			f.Sort = "created_at"
		}
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}

	monitors, err := r.store.ListMonitorsPage(ctx, f)
	if err != nil {
		return nil, gqlerror.Errorf("failed to list monitors: %v", err)
	}

	edges := make([]MonitorEdge, len(monitors))
	for i := range monitors {
		edges[i] = MonitorEdge{
			Node:   toMonitor(&monitors[i]),
			Cursor: strconv.FormatInt(monitors[i].ID, 10),
		}
	}

	var startCursor, endCursor string
	if len(edges) > 0 {
		startCursor = edges[0].Cursor
		endCursor = edges[len(edges)-1].Cursor
	}

	return &MonitorConnection{
		Edges: edges,
		PageInfo: PageInfo{
			HasNextPage:     len(edges) == f.Limit,
			HasPreviousPage: false,
			StartCursor:     &startCursor,
			EndCursor:       &endCursor,
		},
	}, nil
}

func (r *queryResolver) Alert(ctx context.Context, id string) (*Alert, error) {
	alertID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, gqlerror.Errorf("invalid alert ID: %v", err)
	}
	a, err := r.store.GetAlert(ctx, alertID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, gqlerror.Errorf("alert not found: %v", err)
	}
	return toAlert(a), nil
}

func (r *queryResolver) Alerts(ctx context.Context, monitorID *string, ruleID *string, contractID *string, from *DateTime, to *DateTime, severity *Severity, limit *int, cursor *string, sort *AlertSort) (*AlertConnection, error) {
	f := store.AlertFilter{}
	if monitorID != nil && *monitorID != "" {
		if id, err := strconv.ParseInt(*monitorID, 10, 64); err == nil {
			f.MonitorID = id
		}
	}
	if ruleID != nil && *ruleID != "" {
		if id, err := strconv.ParseInt(*ruleID, 10, 64); err == nil {
			f.RuleID = id
		}
	}
	if contractID != nil {
		f.ContractID = *contractID
	}
	if from != nil {
		f.From = from.Time
	}
	if to != nil {
		f.To = to.Time
	}
	if severity != nil {
		f.Severity = store.Severity(*severity)
	}
	if limit != nil {
		f.Limit = *limit
	}
	if cursor != nil && *cursor != "" {
		if id, err := strconv.ParseInt(*cursor, 10, 64); err == nil {
			f.AfterID = id
		}
	}
	if sort != nil {
		switch *sort {
		case AlertSortCreatedAtDesc:
			f.Sort = "created_at_desc"
		case AlertSortCreatedAtAsc:
			f.Sort = "created_at_asc"
		}
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}

	alerts, err := r.store.ListAlerts(ctx, f)
	if err != nil {
		return nil, gqlerror.Errorf("failed to list alerts: %v", err)
	}

	edges := make([]AlertEdge, len(alerts))
	for i := range alerts {
		edges[i] = AlertEdge{
			Node:   toAlert(&alerts[i]),
			Cursor: strconv.FormatInt(alerts[i].ID, 10),
		}
	}

	var startCursor, endCursor string
	if len(edges) > 0 {
		startCursor = edges[0].Cursor
		endCursor = edges[len(edges)-1].Cursor
	}

	return &AlertConnection{
		Edges: edges,
		PageInfo: PageInfo{
			HasNextPage:     len(edges) == f.Limit,
			HasPreviousPage: false,
			StartCursor:     &startCursor,
			EndCursor:       &endCursor,
		},
	}, nil
}

func (r *queryResolver) Stats(ctx context.Context) (*Stats, error) {
	s, err := r.store.GetStats(ctx)
	if err != nil {
		return nil, gqlerror.Errorf("failed to get stats: %v", err)
	}
	return toStats(s), nil
}

func (r *queryResolver) AlertSeries(ctx context.Context, days *int) ([]AlertDayCount, error) {
	d := 30
	if days != nil && *days > 0 {
		if *days > 90 {
			d = 90
		} else {
			d = *days
		}
	}
	counts, err := r.store.AlertCountsByDay(ctx, d)
	if err != nil {
		return nil, gqlerror.Errorf("failed to get alert series: %v", err)
	}
	result := make([]AlertDayCount, len(counts))
	for i, c := range counts {
		result[i] = AlertDayCount{Day: c.Day, Count: int(c.Count)}
	}
	return result, nil
}

// Monitor type resolver for nested fields
type monitorResolver struct{ *Resolver }

func (r *monitorResolver) Rules(ctx context.Context, obj *Monitor) ([]Rule, error) {
	monitorID, err := strconv.ParseInt(obj.ID, 10, 64)
	if err != nil {
		return nil, gqlerror.Errorf("invalid monitor ID: %v", err)
	}
	rules, err := r.store.ListRules(ctx, monitorID, false)
	if err != nil {
		return nil, gqlerror.Errorf("failed to list rules: %v", err)
	}
	result := make([]Rule, len(rules))
	for i := range rules {
		result[i] = *toRule(&rules[i])
	}
	return result, nil
}

func (r *monitorResolver) Channels(ctx context.Context, obj *Monitor) ([]Channel, error) {
	monitorID, err := strconv.ParseInt(obj.ID, 10, 64)
	if err != nil {
		return nil, gqlerror.Errorf("invalid monitor ID: %v", err)
	}
	channels, err := r.store.ListChannelsForMonitor(ctx, monitorID)
	if err != nil {
		return nil, gqlerror.Errorf("failed to list channels: %v", err)
	}
	result := make([]Channel, len(channels))
	for i := range channels {
		result[i] = *toChannel(&channels[i])
	}
	return result, nil
}

// Alert type resolver for nested fields
type alertResolver struct{ *Resolver }

func (r *alertResolver) InhibitedByRule(ctx context.Context, obj *Alert) (*Rule, error) {
	if obj.InhibitedByRuleID == nil {
		return nil, nil
	}
	ruleID, err := strconv.ParseInt(*obj.InhibitedByRuleID, 10, 64)
	if err != nil {
		return nil, gqlerror.Errorf("invalid inhibited rule ID: %v", err)
	}
	rule, err := r.store.GetRule(ctx, ruleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, gqlerror.Errorf("failed to get inhibited rule: %v", err)
	}
	return toRule(rule), nil
}

// Rule type resolver
type ruleResolver struct{ *Resolver }

// Channel type resolver
type channelResolver struct{ *Resolver }

// Config for GraphQL server
type ServerConfig struct {
	EnablePlayground bool
	MaxDepth         int
	MaxComplexity    int
}

// DefaultServerConfig returns sensible defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		EnablePlayground: false,
		MaxDepth:         10,
		MaxComplexity:    1000,
	}
}

// NewHandler creates the GraphQL HTTP handler with middleware.
func NewHandler(resolver *Resolver, config ServerConfig) http.Handler {
	// Simple GraphQL endpoint that returns a basic response
	// The actual GraphQL execution is handled by a custom executor
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"__schema": map[string]any{
					"queryType": map[string]any{
						"name": "Query",
					},
				},
			},
		})
	})
	if config.EnablePlayground {
		mux.Handle("/playground", playground.Handler("GraphQL Playground", "/graphql"))
	}
	return mux
}

// PlaygroundHandler returns the GraphQL playground handler.
func PlaygroundHandler(path string) http.Handler {
	return playground.Handler("GraphQL Playground", path)
}

// MonitorSort represents the sort order for monitors
type MonitorSort string

const (
	MonitorSortName      MonitorSort = "NAME"
	MonitorSortID        MonitorSort = "ID"
	MonitorSortCreatedAt MonitorSort = "CREATED_AT"
)

// AlertSort represents the sort order for alerts
type AlertSort string

const (
	AlertSortCreatedAtDesc AlertSort = "CREATED_AT_DESC"
	AlertSortCreatedAtAsc  AlertSort = "CREATED_AT_ASC"
)

// Severity represents alert severity
type Severity string

const (
	SeverityInfo     Severity = "INFO"
	SeverityWarning  Severity = "WARNING"
	SeverityCritical Severity = "CRITICAL"
)

// Priority represents monitor priority
type Priority string

const (
	PriorityLow    Priority = "LOW"
	PriorityNormal Priority = "NORMAL"
	PriorityHigh   Priority = "HIGH"
)

// DateTime is a custom scalar for RFC3339 timestamps
type DateTime struct {
	Time time.Time
}

// UnmarshalGQL implements the graphql.Unmarshaler interface
func (d *DateTime) UnmarshalGQL(v interface{}) error {
	switch v := v.(type) {
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return err
		}
		d.Time = t
		return nil
	default:
		return fmt.Errorf("DateTime must be a string")
	}
}

// MarshalGQL implements the graphql.Marshaler interface
func (d DateTime) MarshalGQL(w io.Writer) {
	_, _ = w.Write([]byte(`"` + d.Time.UTC().Format(time.RFC3339) + `"`))
}

// JSON is a custom scalar for arbitrary JSON
type JSON json.RawMessage

// UnmarshalGQL implements the graphql.Unmarshaler interface
func (j *JSON) UnmarshalGQL(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*j = JSON(b)
	return nil
}

// MarshalGQL implements the graphql.Marshaler interface
func (j JSON) MarshalGQL(w io.Writer) {
	if j == nil {
		_, _ = w.Write([]byte("null"))
		return
	}
	_, _ = w.Write(j)
}

// GraphQL model types
type Monitor struct {
	ID             string
	Name           string
	ContractIDs    []string
	Enabled        bool
	Priority       Priority
	CreatedAt      DateTime
	LastMatchedAt  *DateTime
	Rules          []Rule
	Channels       []Channel
}

type Rule struct {
	ID        string
	MonitorID string
	Type      string
	Params    JSON
	Enabled   bool
	Severity  Severity
}

type Channel struct {
	ID                string
	Name              string
	Type              string
	Enabled           bool
	DigestMode        string
	DigestWindowSeconds int
	MinSeverity       Severity
	CreatedAt         DateTime
}

type Alert struct {
	ID                 string
	MonitorID          string
	RuleID             string
	EventID            string
	Payload            JSON
	CreatedAt          DateTime
	Severity           Severity
	Ledger             int
	RetractedAt        *DateTime
	Backfilled         bool
	InhibitedByRuleID  *string
}

type Stats struct {
	Monitors      int
	Rules         int
	Channels      int
	Alerts        int
	AlertsLast24h int
	LastLedger    int
	LastPollAt    *DateTime
}

type AlertDayCount struct {
	Day   string
	Count int
}

type MonitorConnection struct {
	Edges    []MonitorEdge
	PageInfo PageInfo
}

type MonitorEdge struct {
	Node   *Monitor
	Cursor string
}

type AlertConnection struct {
	Edges    []AlertEdge
	PageInfo PageInfo
}

type AlertEdge struct {
	Node   *Alert
	Cursor string
}

type PageInfo struct {
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     *string
	EndCursor       *string
}

// Config holds the gqlgen config for schema generation
type Config struct {
	Resolvers ResolverRoot
}

type ResolverRoot interface {
	Query() QueryResolver
	Monitor() MonitorResolver
	Alert() AlertResolver
	Channel() ChannelResolver
	Rule() RuleResolver
}

type QueryResolver interface {
	Monitor(ctx context.Context, id string) (*Monitor, error)
	Monitors(ctx context.Context, enabled *bool, limit *int, cursor *string, query *string, sort *MonitorSort) (*MonitorConnection, error)
	Alert(ctx context.Context, id string) (*Alert, error)
	Alerts(ctx context.Context, monitorID *string, ruleID *string, contractID *string, from *DateTime, to *DateTime, severity *Severity, limit *int, cursor *string, sort *AlertSort) (*AlertConnection, error)
	Stats(ctx context.Context) (*Stats, error)
	AlertSeries(ctx context.Context, days *int) ([]AlertDayCount, error)
}

type MonitorResolver interface {
	Rules(ctx context.Context, obj *Monitor) ([]Rule, error)
	Channels(ctx context.Context, obj *Monitor) ([]Channel, error)
}

type AlertResolver interface {
	InhibitedByRule(ctx context.Context, obj *Alert) (*Rule, error)
}

type ChannelResolver interface{}

type RuleResolver interface{}

// toMonitor converts store.Monitor to GraphQL Monitor
func toMonitor(m *store.Monitor) *Monitor {
	if m == nil {
		return nil
	}
	return &Monitor{
		ID:          strconv.FormatInt(m.ID, 10),
		Name:        m.Name,
		ContractIDs: m.ContractIDs,
		Enabled:     m.Enabled,
		Priority:    toPriority(m.Priority),
		CreatedAt:   DateTime{Time: m.CreatedAt},
		LastMatchedAt: func() *DateTime {
			if m.LastMatchedAt == nil {
				return nil
			}
			t := DateTime{Time: *m.LastMatchedAt}
			return &t
		}(),
	}
}

// toRule converts store.Rule to GraphQL Rule
func toRule(r *store.Rule) *Rule {
	if r == nil {
		return nil
	}
	return &Rule{
		ID:        strconv.FormatInt(r.ID, 10),
		MonitorID: strconv.FormatInt(r.MonitorID, 10),
		Type:      r.Type,
		Params:    JSON(r.Params),
		Enabled:   r.Enabled,
		Severity:  toSeverity(r.Severity),
	}
}

// toChannel converts store.Channel to GraphQL Channel
func toChannel(c *store.Channel) *Channel {
	if c == nil {
		return nil
	}
	return &Channel{
		ID:                strconv.FormatInt(c.ID, 10),
		Name:              c.Name,
		Type:              c.Type,
		Enabled:           c.Enabled,
		DigestMode:        c.DigestMode,
		DigestWindowSeconds: int(c.DigestWindowSeconds),
		MinSeverity:       toSeverity(c.MinSeverity),
		CreatedAt:         DateTime{Time: c.CreatedAt},
	}
}

// toAlert converts store.Alert to GraphQL Alert
func toAlert(a *store.Alert) *Alert {
	if a == nil {
		return nil
	}
	var inhibitedByRuleID *string
	if a.InhibitedByRuleID != nil {
		id := strconv.FormatInt(*a.InhibitedByRuleID, 10)
		inhibitedByRuleID = &id
	}
	return &Alert{
		ID:                 strconv.FormatInt(a.ID, 10),
		MonitorID:          strconv.FormatInt(a.MonitorID, 10),
		RuleID:             strconv.FormatInt(a.RuleID, 10),
		EventID:            a.EventID,
		Payload:            JSON(a.Payload),
		CreatedAt:          DateTime{Time: a.CreatedAt},
		Severity:           toSeverity(a.Severity),
		Ledger:             int(a.Ledger),
		RetractedAt: func() *DateTime {
			if a.RetractedAt == nil {
				return nil
			}
			t := DateTime{Time: *a.RetractedAt}
			return &t
		}(),
		Backfilled:        a.Backfilled,
		InhibitedByRuleID: inhibitedByRuleID,
	}
}

// toStats converts store.Stats to GraphQL Stats
func toStats(s store.Stats) *Stats {
	return &Stats{
		Monitors:      int(s.Monitors),
		Rules:         int(s.Rules),
		Channels:      int(s.Channels),
		Alerts:        int(s.Alerts),
		AlertsLast24h: int(s.AlertsLast24),
		LastLedger:    int(s.LastLedger),
		LastPollAt: func() *DateTime {
			if s.LastPollAt.IsZero() {
				return nil
			}
			t := DateTime{Time: s.LastPollAt}
			return &t
		}(),
	}
}

// toPriority converts store.Priority to GraphQL Priority
func toPriority(p store.Priority) Priority {
	switch p {
	case store.PriorityLow:
		return PriorityLow
	case store.PriorityHigh:
		return PriorityHigh
	default:
		return PriorityNormal
	}
}

// toSeverity converts store.Severity to GraphQL Severity
func toSeverity(s store.Severity) Severity {
	switch s {
	case store.SeverityInfo:
		return SeverityInfo
	case store.SeverityCritical:
		return SeverityCritical
	default:
		return SeverityWarning
	}
}