package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakeSoroBeaconServer simulates SoroBeacon's REST API (/api/v1) in memory over
// httptest so every Terraform resource can be exercised through Create, Read,
// Update, ImportState, and Delete (including 404 drift removal) with zero
// external dependencies (#609).
type fakeSoroBeaconServer struct {
	mu          sync.Mutex
	nextID      int64
	monitors    map[int64]apiMonitor
	rules       map[int64][]apiRule
	channels    map[int64]apiChannel
	escalations map[int64]apiEscalationPolicy
	windows     map[int64]apiMaintenanceWindow
	inhibitions map[string]apiInhibition
}

func newFakeSoroBeaconServer() *fakeSoroBeaconServer {
	return &fakeSoroBeaconServer{
		nextID:      10,
		monitors:    map[int64]apiMonitor{},
		rules:       map[int64][]apiRule{},
		channels:    map[int64]apiChannel{},
		escalations: map[int64]apiEscalationPolicy{},
		windows:     map[int64]apiMaintenanceWindow{},
		inhibitions: map[string]apiInhibition{},
	}
}

func (s *fakeSoroBeaconServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path

	switch {
	case path == "/monitors" && r.Method == http.MethodPost:
		var body struct {
			Name        string   `json:"name"`
			ContractIDs []string `json:"contract_ids"`
			Enabled     bool     `json:"enabled"`
			ChannelIDs  []int64  `json:"channel_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.nextID++
		m := apiMonitor{
			ID:          s.nextID,
			Name:        body.Name,
			ContractIDs: body.ContractIDs,
			Enabled:     body.Enabled,
			ChannelIDs:  body.ChannelIDs,
		}
		s.monitors[m.ID] = m
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(m)

	case strings.HasPrefix(path, "/monitors/") && strings.HasSuffix(path, "/escalation"):
		midStr := strings.TrimSuffix(strings.TrimPrefix(path, "/monitors/"), "/escalation")
		mid, _ := strconv.ParseInt(midStr, 10, 64)
		switch r.Method {
		case http.MethodPut:
			var body struct {
				Steps []struct {
					Delay      string  `json:"delay"`
					ChannelIDs []int64 `json:"channel_ids"`
				} `json:"steps"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			steps := make([]apiEscalationStep, len(body.Steps))
			for i, st := range body.Steps {
				steps[i] = apiEscalationStep{
					Position:   i,
					Delay:      normalizeDurationString(st.Delay),
					ChannelIDs: st.ChannelIDs,
				}
			}
			pol, exists := s.escalations[mid]
			if !exists {
				s.nextID++
				pol.ID = s.nextID
				pol.MonitorID = mid
			}
			pol.Steps = steps
			s.escalations[mid] = pol
			_ = json.NewEncoder(w).Encode(pol)
		case http.MethodGet:
			pol, ok := s.escalations[mid]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"escalation policy not found","code":"NOT_FOUND"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(pol)
		case http.MethodDelete:
			delete(s.escalations, mid)
			w.WriteHeader(http.StatusNoContent)
		}

	case strings.HasPrefix(path, "/monitors/") && strings.Contains(path, "/rules"):
		rest := strings.TrimPrefix(path, "/monitors/")
		parts := strings.Split(rest, "/")
		mid, _ := strconv.ParseInt(parts[0], 10, 64)
		switch {
		case len(parts) == 2 && r.Method == http.MethodPost:
			var body struct {
				Type    string          `json:"type"`
				Params  json.RawMessage `json:"params"`
				Enabled bool            `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.nextID++
			rule := apiRule{
				ID:        s.nextID,
				MonitorID: mid,
				Type:      body.Type,
				Params:    body.Params,
				Enabled:   body.Enabled,
			}
			s.rules[mid] = append(s.rules[mid], rule)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(rule)
		case len(parts) == 2 && r.Method == http.MethodGet:
			if _, ok := s.monitors[mid]; !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"monitor not found","code":"NOT_FOUND"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(s.rules[mid])
		case len(parts) == 3 && r.Method == http.MethodPatch:
			rid, _ := strconv.ParseInt(parts[2], 10, 64)
			var body struct {
				Type    string          `json:"type"`
				Params  json.RawMessage `json:"params"`
				Enabled bool            `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for i, rule := range s.rules[mid] {
				if rule.ID == rid {
					rule.Type = body.Type
					rule.Params = body.Params
					rule.Enabled = body.Enabled
					s.rules[mid][i] = rule
					_ = json.NewEncoder(w).Encode(rule)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case len(parts) == 3 && r.Method == http.MethodDelete:
			rid, _ := strconv.ParseInt(parts[2], 10, 64)
			kept := s.rules[mid][:0]
			for _, rule := range s.rules[mid] {
				if rule.ID != rid {
					kept = append(kept, rule)
				}
			}
			s.rules[mid] = kept
			w.WriteHeader(http.StatusNoContent)
		}

	case strings.HasPrefix(path, "/monitors/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/monitors/"), 10, 64)
		switch r.Method {
		case http.MethodGet:
			m, ok := s.monitors[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"monitor not found","code":"NOT_FOUND"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(m)
		case http.MethodPatch:
			m := s.monitors[id]
			var body struct {
				Name        string   `json:"name"`
				ContractIDs []string `json:"contract_ids"`
				Enabled     bool     `json:"enabled"`
				ChannelIDs  []int64  `json:"channel_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.Name = body.Name
			m.ContractIDs = body.ContractIDs
			m.Enabled = body.Enabled
			m.ChannelIDs = body.ChannelIDs
			s.monitors[id] = m
			_ = json.NewEncoder(w).Encode(m)
		case http.MethodDelete:
			delete(s.monitors, id)
			w.WriteHeader(http.StatusNoContent)
		}

	case path == "/channels" && r.Method == http.MethodPost:
		var body struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Enabled bool   `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.nextID++
		ch := apiChannel{
			ID:      s.nextID,
			Name:    body.Name,
			Type:    body.Type,
			Enabled: body.Enabled,
		}
		s.channels[ch.ID] = ch
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ch)

	case strings.HasPrefix(path, "/channels/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/channels/"), 10, 64)
		switch r.Method {
		case http.MethodGet:
			ch, ok := s.channels[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"channel not found","code":"NOT_FOUND"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(ch)
		case http.MethodPatch:
			ch := s.channels[id]
			var body struct {
				Name    string `json:"name"`
				Type    string `json:"type"`
				Enabled bool   `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ch.Name = body.Name
			ch.Type = body.Type
			ch.Enabled = body.Enabled
			s.channels[id] = ch
			_ = json.NewEncoder(w).Encode(ch)
		case http.MethodDelete:
			delete(s.channels, id)
			w.WriteHeader(http.StatusNoContent)
		}

	case path == "/maintenance" && r.Method == http.MethodPost:
		var body struct {
			Reason     string  `json:"reason"`
			Scope      string  `json:"scope"`
			MonitorID  *int64  `json:"monitor_id"`
			ContractID *string `json:"contract_id"`
			StartAt    string  `json:"start_at"`
			EndAt      string  `json:"end_at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		startAt, _ := time.Parse(time.RFC3339, body.StartAt)
		endAt, _ := time.Parse(time.RFC3339, body.EndAt)
		s.nextID++
		mw := apiMaintenanceWindow{
			ID:         s.nextID,
			Reason:     body.Reason,
			Scope:      body.Scope,
			MonitorID:  body.MonitorID,
			ContractID: body.ContractID,
			StartAt:    startAt.UTC(),
			EndAt:      endAt.UTC(),
		}
		s.windows[mw.ID] = mw
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(mw)

	case strings.HasPrefix(path, "/maintenance/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/maintenance/"), 10, 64)
		switch r.Method {
		case http.MethodGet:
			mw, ok := s.windows[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"maintenance window not found","code":"NOT_FOUND"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(mw)
		case http.MethodPatch:
			var body struct {
				Reason     string  `json:"reason"`
				Scope      string  `json:"scope"`
				MonitorID  *int64  `json:"monitor_id"`
				ContractID *string `json:"contract_id"`
				StartAt    string  `json:"start_at"`
				EndAt      string  `json:"end_at"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			startAt, _ := time.Parse(time.RFC3339, body.StartAt)
			endAt, _ := time.Parse(time.RFC3339, body.EndAt)
			mw := apiMaintenanceWindow{
				ID:         id,
				Reason:     body.Reason,
				Scope:      body.Scope,
				MonitorID:  body.MonitorID,
				ContractID: body.ContractID,
				StartAt:    startAt.UTC(),
				EndAt:      endAt.UTC(),
			}
			s.windows[id] = mw
			_ = json.NewEncoder(w).Encode(mw)
		case http.MethodDelete:
			delete(s.windows, id)
			w.WriteHeader(http.StatusNoContent)
		}

	case path == "/inhibitions" && r.Method == http.MethodPost:
		var inh apiInhibition
		_ = json.NewDecoder(r.Body).Decode(&inh)
		if inh.FiringWindowSeconds <= 0 {
			inh.FiringWindowSeconds = 600
		}
		s.inhibitions[formatInhibitionID(inh.SourceRuleID, inh.TargetRuleID)] = inh
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(inh)

	case path == "/inhibitions" && r.Method == http.MethodGet:
		list := make([]apiInhibition, 0, len(s.inhibitions))
		for _, inh := range s.inhibitions {
			list = append(list, inh)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"inhibitions": list})

	case strings.HasPrefix(path, "/inhibitions/") && r.Method == http.MethodDelete:
		key := strings.TrimPrefix(path, "/inhibitions/")
		delete(s.inhibitions, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func configureResource(t *testing.T, r resource.Resource, client *apiClient) schema.Schema {
	t.Helper()
	ctx := context.Background()
	var schResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schResp)
	if schResp.Diagnostics.HasError() {
		t.Fatalf("Schema error: %v", schResp.Diagnostics)
	}
	if cfgR, ok := r.(resource.ResourceWithConfigure); ok {
		var cfgResp resource.ConfigureResponse
		cfgR.Configure(ctx, resource.ConfigureRequest{ProviderData: client}, &cfgResp)
		if cfgResp.Diagnostics.HasError() {
			t.Fatalf("Configure error: %v", cfgResp.Diagnostics)
		}
	}
	return schResp.Schema
}

func makeState(t *testing.T, sch schema.Schema, model any) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	st := tfsdk.State{
		Schema: sch,
		Raw:    tftypes.NewValue(sch.Type().TerraformType(ctx), nil),
	}
	if model != nil {
		diags := st.Set(ctx, model)
		if diags.HasError() {
			t.Fatalf("set state: %v", diags)
		}
	}
	return st
}

func makePlan(t *testing.T, sch schema.Schema, model any) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	pl := tfsdk.Plan{
		Schema: sch,
		Raw:    tftypes.NewValue(sch.Type().TerraformType(ctx), nil),
	}
	diags := pl.Set(ctx, model)
	if diags.HasError() {
		t.Fatalf("set plan: %v", diags)
	}
	return pl
}

func TestChannelConfigAttributeMarkedSensitive(t *testing.T) {
	r := NewChannelResource()
	var schResp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &schResp)
	cfgAttr, ok := schResp.Schema.Attributes["config"]
	if !ok {
		t.Fatal("channel schema missing config attribute")
	}
	if !cfgAttr.IsSensitive() {
		t.Fatal("channel config attribute MUST be marked Sensitive so plan output redacts secrets")
	}
}

func TestAllResourcesRoundTripUpdateImportAnd404Removal(t *testing.T) {
	ctx := context.Background()
	fake := newFakeSoroBeaconServer()
	srv := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	defer srv.Close()

	client := &apiClient{
		endpoint:   srv.URL,
		token:      "test-token",
		httpClient: srv.Client(),
	}

	t.Run("sorobeacon_channel create read update import and 404 removal", func(t *testing.T) {
		r := NewChannelResource()
		sch := configureResource(t, r, client)

		planModel := ChannelResourceModel{
			Name:    types.StringValue("ops-slack"),
			Type:    types.StringValue("slack"),
			Config:  types.StringValue(`{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`),
			Enabled: types.BoolValue(true),
		}
		var createResp resource.CreateResponse
		createResp.State = makeState(t, sch, nil)
		r.Create(ctx, resource.CreateRequest{Plan: makePlan(t, sch, planModel)}, &createResp)
		if createResp.Diagnostics.HasError() {
			t.Fatalf("Create failed: %v", createResp.Diagnostics)
		}

		var created ChannelResourceModel
		_ = createResp.State.Get(ctx, &created)
		if created.ID.ValueInt64() <= 0 {
			t.Fatalf("expected positive ID, got %d", created.ID.ValueInt64())
		}

		// Read back and verify zero diff against created state (including preserved sensitive config).
		var readResp resource.ReadResponse
		readResp.State = createResp.State
		r.Read(ctx, resource.ReadRequest{State: createResp.State}, &readResp)
		if readResp.Diagnostics.HasError() {
			t.Fatalf("Read failed: %v", readResp.Diagnostics)
		}
		var readBack ChannelResourceModel
		_ = readResp.State.Get(ctx, &readBack)
		if readBack != created {
			t.Fatalf("create-then-read diff: got %+v, want %+v", readBack, created)
		}

		// Update name and enabled.
		updatedPlan := readBack
		updatedPlan.Name = types.StringValue("ops-slack-renamed")
		updatedPlan.Enabled = types.BoolValue(false)
		var updResp resource.UpdateResponse
		updResp.State = readResp.State
		r.Update(ctx, resource.UpdateRequest{Plan: makePlan(t, sch, updatedPlan), State: readResp.State}, &updResp)
		if updResp.Diagnostics.HasError() {
			t.Fatalf("Update failed: %v", updResp.Diagnostics)
		}

		// ImportState.
		impR := r.(resource.ResourceWithImportState)
		var impResp resource.ImportStateResponse
		impResp.State = makeState(t, sch, nil)
		impR.ImportState(ctx, resource.ImportStateRequest{ID: strconv.FormatInt(created.ID.ValueInt64(), 10)}, &impResp)
		if impResp.Diagnostics.HasError() {
			t.Fatalf("ImportState failed: %v", impResp.Diagnostics)
		}

		// Delete then verify Read on 404 removes resource from state rather than failing.
		var delResp resource.DeleteResponse
		r.Delete(ctx, resource.DeleteRequest{State: updResp.State}, &delResp)
		if delResp.Diagnostics.HasError() {
			t.Fatalf("Delete failed: %v", delResp.Diagnostics)
		}

		var read404Resp resource.ReadResponse
		read404Resp.State = updResp.State
		r.Read(ctx, resource.ReadRequest{State: updResp.State}, &read404Resp)
		if read404Resp.Diagnostics.HasError() {
			t.Fatalf("Read on 404 must not error: %v", read404Resp.Diagnostics)
		}
		if !read404Resp.State.Raw.IsNull() {
			t.Fatal("Read on 404 must remove resource from state")
		}
	})

	t.Run("sorobeacon_monitor and sorobeacon_rule and sorobeacon_escalation_policy and sorobeacon_inhibition round-trip", func(t *testing.T) {
		monR := NewMonitorResource()
		monSch := configureResource(t, monR, client)

		contractsList, _ := types.ListValue(types.StringType, []attr.Value{types.StringValue("CCONTRACT1")})
		channelsList, _ := types.ListValue(types.Int64Type, []attr.Value{types.Int64Value(1)})
		monPlan := MonitorResourceModel{
			Name:        types.StringValue("core-monitor"),
			ContractIDs: contractsList,
			Enabled:     types.BoolValue(true),
			ChannelIDs:  channelsList,
		}
		var monCreate resource.CreateResponse
		monCreate.State = makeState(t, monSch, nil)
		monR.Create(ctx, resource.CreateRequest{Plan: makePlan(t, monSch, monPlan)}, &monCreate)
		if monCreate.Diagnostics.HasError() {
			t.Fatalf("Create monitor: %v", monCreate.Diagnostics)
		}
		var monCreated MonitorResourceModel
		_ = monCreate.State.Get(ctx, &monCreated)

		var monRead resource.ReadResponse
		monRead.State = monCreate.State
		monR.Read(ctx, resource.ReadRequest{State: monCreate.State}, &monRead)
		var monReadBack MonitorResourceModel
		_ = monRead.State.Get(ctx, &monReadBack)
		if !monReadBack.Name.Equal(monCreated.Name) || !monReadBack.ContractIDs.Equal(monCreated.ContractIDs) {
			t.Fatalf("monitor create-then-read mismatch: %+v vs %+v", monReadBack, monCreated)
		}

		// Create two rules on the monitor.
		ruleR := NewRuleResource()
		ruleSch := configureResource(t, ruleR, client)
		createRule := func(eventName string) RuleResourceModel {
			rp := RuleResourceModel{
				MonitorID: monCreated.ID,
				Type:      types.StringValue("event_emitted"),
				Params:    types.StringValue(`{"event_name":"` + eventName + `"}`),
				Enabled:   types.BoolValue(true),
			}
			var rc resource.CreateResponse
			rc.State = makeState(t, ruleSch, nil)
			ruleR.Create(ctx, resource.CreateRequest{Plan: makePlan(t, ruleSch, rp)}, &rc)
			if rc.Diagnostics.HasError() {
				t.Fatalf("Create rule: %v", rc.Diagnostics)
			}
			var out RuleResourceModel
			_ = rc.State.Get(ctx, &out)
			return out
		}
		rule1 := createRule("pause")
		rule2 := createRule("transfer")

		// Escalation policy: Create -> Read (no diff) -> Update -> Import -> Delete -> 404 Read removes state.
		escR := NewEscalationPolicyResource()
		escSch := configureResource(t, escR, client)
		stepChs, _ := types.ListValue(types.Int64Type, []attr.Value{types.Int64Value(1)})
		escPlan := EscalationPolicyResourceModel{
			MonitorID: monCreated.ID,
			Steps: []EscalationStepModel{
				{Delay: types.StringValue("0s"), ChannelIDs: stepChs},
				{Delay: types.StringValue("5m0s"), ChannelIDs: stepChs},
			},
		}
		var escCreate resource.CreateResponse
		escCreate.State = makeState(t, escSch, nil)
		escR.Create(ctx, resource.CreateRequest{Plan: makePlan(t, escSch, escPlan)}, &escCreate)
		if escCreate.Diagnostics.HasError() {
			t.Fatalf("Create escalation policy: %v", escCreate.Diagnostics)
		}
		var escCreated EscalationPolicyResourceModel
		_ = escCreate.State.Get(ctx, &escCreated)

		var escRead resource.ReadResponse
		escRead.State = escCreate.State
		escR.Read(ctx, resource.ReadRequest{State: escCreate.State}, &escRead)
		if escRead.Diagnostics.HasError() {
			t.Fatalf("Read escalation policy: %v", escRead.Diagnostics)
		}
		var escReadBack EscalationPolicyResourceModel
		_ = escRead.State.Get(ctx, &escReadBack)
		if len(escReadBack.Steps) != 2 || !escReadBack.Steps[1].Delay.Equal(escCreated.Steps[1].Delay) {
			t.Fatalf("escalation create-then-read diff: %+v vs %+v", escReadBack, escCreated)
		}

		// Update escalation policy steps.
		escPlan.Steps = escPlan.Steps[:1]
		var escUpd resource.UpdateResponse
		escUpd.State = escRead.State
		escR.Update(ctx, resource.UpdateRequest{Plan: makePlan(t, escSch, escPlan), State: escRead.State}, &escUpd)
		if escUpd.Diagnostics.HasError() {
			t.Fatalf("Update escalation policy: %v", escUpd.Diagnostics)
		}

		// Import escalation policy by monitor_id.
		var escImp resource.ImportStateResponse
		escImp.State = makeState(t, escSch, nil)
		escR.(resource.ResourceWithImportState).ImportState(ctx,
			resource.ImportStateRequest{ID: strconv.FormatInt(monCreated.ID.ValueInt64(), 10)}, &escImp)
		if escImp.Diagnostics.HasError() {
			t.Fatalf("Import escalation policy: %v", escImp.Diagnostics)
		}

		var escDel resource.DeleteResponse
		escR.Delete(ctx, resource.DeleteRequest{State: escUpd.State}, &escDel)
		var escRead404 resource.ReadResponse
		escRead404.State = escUpd.State
		escR.Read(ctx, resource.ReadRequest{State: escUpd.State}, &escRead404)
		if !escRead404.State.Raw.IsNull() {
			t.Fatal("deleted escalation policy must be removed from state on 404 Read")
		}

		// Inhibition resource: Create -> Read (no diff) -> Import -> Delete -> 404 Read removes state.
		inhR := NewInhibitionResource()
		inhSch := configureResource(t, inhR, client)
		inhPlan := InhibitionResourceModel{
			SourceRuleID:        rule1.ID,
			TargetRuleID:        rule2.ID,
			FiringWindowSeconds: types.Int64Value(300),
		}
		var inhCreate resource.CreateResponse
		inhCreate.State = makeState(t, inhSch, nil)
		inhR.Create(ctx, resource.CreateRequest{Plan: makePlan(t, inhSch, inhPlan)}, &inhCreate)
		if inhCreate.Diagnostics.HasError() {
			t.Fatalf("Create inhibition: %v", inhCreate.Diagnostics)
		}
		var inhCreated InhibitionResourceModel
		_ = inhCreate.State.Get(ctx, &inhCreated)

		var inhRead resource.ReadResponse
		inhRead.State = inhCreate.State
		inhR.Read(ctx, resource.ReadRequest{State: inhCreate.State}, &inhRead)
		var inhReadBack InhibitionResourceModel
		_ = inhRead.State.Get(ctx, &inhReadBack)
		if inhReadBack != inhCreated {
			t.Fatalf("inhibition create-then-read diff: %+v vs %+v", inhReadBack, inhCreated)
		}

		var inhImp resource.ImportStateResponse
		inhImp.State = makeState(t, inhSch, nil)
		inhR.(resource.ResourceWithImportState).ImportState(ctx,
			resource.ImportStateRequest{ID: inhCreated.ID.ValueString()}, &inhImp)
		if inhImp.Diagnostics.HasError() {
			t.Fatalf("Import inhibition: %v", inhImp.Diagnostics)
		}

		var inhDel resource.DeleteResponse
		inhR.Delete(ctx, resource.DeleteRequest{State: inhRead.State}, &inhDel)
		var inhRead404 resource.ReadResponse
		inhRead404.State = inhRead.State
		inhR.Read(ctx, resource.ReadRequest{State: inhRead.State}, &inhRead404)
		if !inhRead404.State.Raw.IsNull() {
			t.Fatal("deleted inhibition must be removed from state on Read")
		}
	})

	t.Run("sorobeacon_maintenance_window create read update import and 404 removal", func(t *testing.T) {
		mwR := NewMaintenanceWindowResource()
		mwSch := configureResource(t, mwR, client)

		mwPlan := MaintenanceWindowResourceModel{
			Reason:     types.StringValue("planned contract upgrade"),
			Scope:      types.StringValue("contract"),
			MonitorID:  types.Int64Null(),
			ContractID: types.StringValue("CCONTRACT1"),
			StartAt:    types.StringValue("2026-11-06T12:00:00Z"),
			EndAt:      types.StringValue("2026-11-06T14:00:00Z"),
		}
		var mwCreate resource.CreateResponse
		mwCreate.State = makeState(t, mwSch, nil)
		mwR.Create(ctx, resource.CreateRequest{Plan: makePlan(t, mwSch, mwPlan)}, &mwCreate)
		if mwCreate.Diagnostics.HasError() {
			t.Fatalf("Create maintenance window: %v", mwCreate.Diagnostics)
		}
		var mwCreated MaintenanceWindowResourceModel
		_ = mwCreate.State.Get(ctx, &mwCreated)

		var mwRead resource.ReadResponse
		mwRead.State = mwCreate.State
		mwR.Read(ctx, resource.ReadRequest{State: mwCreate.State}, &mwRead)
		var mwReadBack MaintenanceWindowResourceModel
		_ = mwRead.State.Get(ctx, &mwReadBack)
		if mwReadBack != mwCreated {
			t.Fatalf("maintenance window create-then-read diff: %+v vs %+v", mwReadBack, mwCreated)
		}

		// Update reason and scope to global (clearing contract_id).
		mwUpdPlan := mwReadBack
		mwUpdPlan.Reason = types.StringValue("instance maintenance")
		mwUpdPlan.Scope = types.StringValue("global")
		mwUpdPlan.ContractID = types.StringNull()
		var mwUpd resource.UpdateResponse
		mwUpd.State = mwRead.State
		mwR.Update(ctx, resource.UpdateRequest{Plan: makePlan(t, mwSch, mwUpdPlan), State: mwRead.State}, &mwUpd)
		if mwUpd.Diagnostics.HasError() {
			t.Fatalf("Update maintenance window: %v", mwUpd.Diagnostics)
		}
		var mwUpdated MaintenanceWindowResourceModel
		_ = mwUpd.State.Get(ctx, &mwUpdated)
		if !mwUpdated.ContractID.IsNull() || mwUpdated.Scope.ValueString() != "global" {
			t.Fatalf("updated maintenance window state unexpected: %+v", mwUpdated)
		}

		// ImportState.
		var mwImp resource.ImportStateResponse
		mwImp.State = makeState(t, mwSch, nil)
		mwR.(resource.ResourceWithImportState).ImportState(ctx,
			resource.ImportStateRequest{ID: strconv.FormatInt(mwCreated.ID.ValueInt64(), 10)}, &mwImp)
		if mwImp.Diagnostics.HasError() {
			t.Fatalf("Import maintenance window: %v", mwImp.Diagnostics)
		}

		// Delete and verify 404 removes from state.
		var mwDel resource.DeleteResponse
		mwR.Delete(ctx, resource.DeleteRequest{State: mwUpd.State}, &mwDel)
		var mwRead404 resource.ReadResponse
		mwRead404.State = mwUpd.State
		mwR.Read(ctx, resource.ReadRequest{State: mwUpd.State}, &mwRead404)
		if !mwRead404.State.Raw.IsNull() {
			t.Fatal("deleted maintenance window must be removed from state on 404 Read")
		}
	})
}
