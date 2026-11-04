package rules

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParamsFromForm pins the builder's coercion: a number field is a number,
// a value too large for float64 keeps every digit, and a field that is not a
// number is reported by name rather than taking the whole params blob with
// it.
func TestParamsFromForm(t *testing.T) {
	schema := []FieldSchema{
		{Name: "event_name", Type: "string"},
		{Name: "count", Type: "number"},
		{Name: "threshold", Type: "number"},
		{Name: "enabled", Type: "boolean"},
	}
	tests := []struct {
		name       string
		values     map[string]string
		wantParams string
		wantErr    string // the field expected in the error, empty for no error
	}{
		{
			name:       "number field becomes a number",
			values:     map[string]string{"event_name": "transfer", "count": "50"},
			wantParams: `{"count":50,"event_name":"transfer"}`,
		},
		{
			name:       "integer beyond float64 precision keeps every digit",
			values:     map[string]string{"threshold": "123456789012345678901234567890"},
			wantParams: `{"threshold":123456789012345678901234567890}`,
		},
		{
			name:       "decimal and exponent are numbers",
			values:     map[string]string{"threshold": "1.5e3"},
			wantParams: `{"threshold":1.5e3}`,
		},
		{
			name:    "non-numeric value is reported by field name",
			values:  map[string]string{"event_name": "transfer", "count": "many"},
			wantErr: "count",
		},
		{
			name:    "a number field with a thousands separator is not a number",
			values:  map[string]string{"count": "1,000"},
			wantErr: "count",
		},
		{
			name:       "boolean field becomes a boolean",
			values:     map[string]string{"enabled": "true"},
			wantParams: `{"enabled":true}`,
		},
		{
			name:       "false becomes the boolean false",
			values:     map[string]string{"enabled": "false"},
			wantParams: `{"enabled":false}`,
		},
		{
			name:    "non-boolean value is reported by field name",
			values:  map[string]string{"event_name": "transfer", "enabled": "yes"},
			wantErr: "enabled",
		},
		{
			name:       "empty and absent fields are left out",
			values:     map[string]string{"event_name": "", "count": ""},
			wantParams: `{}`,
		},
		{
			name:       "a field the schema does not know is ignored",
			values:     map[string]string{"nope": "x"},
			wantParams: `{}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := ParamsFromForm(schema, tt.values)
			if tt.wantErr != "" {
				require.Error(t, err, "expected an error naming %q", tt.wantErr)
				var fields FieldErrors
				require.ErrorAs(t, err, &fields)
				require.NotEmpty(t, fields)
				assert.Equal(t, tt.wantErr, fields[0].Field)
				assert.Nil(t, raw, "no params are returned when a field is malformed")
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tt.wantParams, string(raw))
		})
	}
}

// TestParamsFromFormReportsEveryMalformedField: one bad field must not hide
// the next one, and the fields that are fine must still be in the params the
// caller gets back.
func TestParamsFromFormReportsEveryMalformedField(t *testing.T) {
	schema := []FieldSchema{
		{Name: "count", Type: "number"},
		{Name: "position", Type: "number"},
		{Name: "event_name", Type: "string"},
	}
	raw, err := ParamsFromForm(schema, map[string]string{
		"count": "many", "position": "top", "event_name": "transfer",
	})
	require.Error(t, err)
	var fields FieldErrors
	require.ErrorAs(t, err, &fields)
	require.Len(t, fields, 2)
	assert.Equal(t, "count", fields[0].Field)
	assert.Equal(t, "position", fields[1].Field)
	assert.Nil(t, raw)
}

// TestParamsFromFormKeepsLargeIntegersExact: the number is carried as text
// all the way into the params, so a threshold beyond float64's precision is
// not rounded on the way to the store.
func TestParamsFromFormKeepsLargeIntegersExact(t *testing.T) {
	raw, err := ParamsFromForm([]FieldSchema{{Name: "threshold", Type: "number"}},
		map[string]string{"threshold": "9007199254740993"})
	require.NoError(t, err)
	var got map[string]json.Number
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "9007199254740993", got["threshold"].String())
}

// formFixtures are the required values a generic schema cannot invent: a
// valid contract id, a real account strkey, a duration, and so on. Optional
// fields use the schema's own Default, which is the path the dashboard takes
// when the operator leaves them alone.
var formFixtures = map[string]map[string]string{
	TypeEventEmitted: {
		"event_name": "transfer",
	},
	TypeContractAllowlist: {
		"contract_ids": `["` + allowTreasury + `"]`,
		"exclude":      "true",
	},
	TypeValueThreshold: {
		"comparison": "gt",
		"threshold":  "1",
	},
	TypeEventNameGlob: {
		"patterns": `["swap_*"]`,
	},
	TypeNumericRange: {
		"min": "1",
	},
	TypeTokenEvent: {
		"event": "transfer",
	},
	TypeFrequencyThreshold: {
		"count":  "1",
		"window": "1m",
	},
	TypeTopicRegex: {
		"pattern": "^swap_",
	},
	TypeComposite: {
		"rules": `[{"type":"event_emitted","params":{"event_name":"transfer"}}]`,
	},
	TypeTimeWindow: {
		"start":   "09:00",
		"end":     "17:00",
		"outside": "true",
	},
	TypeAddressWatchlist: {
		"addresses": `["` + wlAlice + `"]`,
	},
	TypeTopicPosition: {
		"position": "1",
		"equals":   "transfer",
	},
	TypeBaselineDeviation: {
		"window":      "1h",
		"sensitivity": "3",
	},
	TypeAbsenceOfEvent: {
		"event_name": "transfer",
		"window":     "5m",
	},
}

// TestEveryRuleTypeFormParamsValidate is the guard the boolean bug needed:
// take each registered type's own schema, fill required fields from a fixture
// and every optional field from its default, coerce through ParamsFromForm,
// and require the registry to accept the result. A boolean accidentally left
// as a string fails here before a user ever sees a 400 from the dashboard.
func TestEveryRuleTypeFormParamsValidate(t *testing.T) {
	reg := NewRegistry()
	schemas := reg.AllSchemas()
	require.Len(t, schemas, len(reg.Types()), "every registered rule type must declare a schema")

	for typ, schema := range schemas {
		t.Run(typ, func(t *testing.T) {
			values := make(map[string]string, len(schema))
			for _, f := range schema {
				if v, ok := formFixtures[typ][f.Name]; ok {
					values[f.Name] = v
					continue
				}
				if f.Default != "" {
					values[f.Name] = f.Default
					continue
				}
				if f.Required {
					t.Fatalf("no form fixture for required field %s.%s", typ, f.Name)
				}
			}
			raw, err := ParamsFromForm(schema, values)
			require.NoError(t, err)

			// The dashboard parses object fields (JSON arrays and maps typed
			// into a textarea) after ParamsFromForm, so mirror that step
			// before handing the blob to the registry.
			var params map[string]any
			require.NoError(t, json.Unmarshal(raw, &params))
			for _, f := range schema {
				if f.Type != "object" || values[f.Name] == "" {
					continue
				}
				var obj any
				require.NoError(t, json.Unmarshal([]byte(values[f.Name]), &obj), "field %s", f.Name)
				params[f.Name] = obj
			}
			raw, err = json.Marshal(params)
			require.NoError(t, err)
			require.NoError(t, reg.Validate(typ, raw), "params: %s", raw)
		})
	}

	for typ := range formFixtures {
		_, ok := schemas[typ]
		require.True(t, ok, "form fixture names unknown rule type %q", typ)
	}
}

// --- schema guards ---

// schemaParamsStructs maps every type NewRegistry registers to the params
// struct its evaluator unmarshals — including the absence type, which is
// registered separately. The mapping lives here, in package rules, because
// the structs are unexported: only a test inside the package can compare
// them against the schema the dashboard's rule builder renders.
var schemaParamsStructs = map[string]any{
	TypeEventEmitted:       eventEmittedParams{},
	TypeValueThreshold:     valueThresholdParams{},
	TypeContractAllowlist:  contractAllowlistParams{},
	TypeEventNameGlob:      eventNameGlobParams{},
	TypeNumericRange:       numericRangeParams{},
	TypeTokenSupplyChange:  tokenSupplyChangeParams{},
	TypeTokenEvent:         tokenEventParams{},
	TypeSelfTransfer:       selfTransferParams{},
	TypeTimeWindow:         timeWindowParams{},
	TypeFrequencyThreshold: frequencyThresholdParams{},
	TypeComposite:          compositeParams{},
	TypeTopicRegex:         topicRegexParams{},
	TypeAddressWatchlist:   addressWatchlistParams{},
	TypeTopicPosition:      topicPositionParams{},
	TypeBaselineDeviation:  baselineDeviationParams{},
	TypeAbsenceOfEvent:     absenceParams{},
}

// schemaOnlyFields are fields the schema declares that no params struct
// holds: the cross-cutting cooldown rides in every rule's params but is
// parsed by ParseCooldown rather than by the evaluator's own struct, so the
// struct/schema comparison below would otherwise flag it in both directions.
var schemaOnlyFields = map[string]bool{CooldownParam: true}

// TestRegistryParamSchemaCoversEveryType pins the registry half of the
// builder's contract: every registered type — absence types included —
// answers ParamSchema with a usable form description, and AllSchemas returns
// exactly one entry per registered type, agreeing with ParamSchema. A new
// rule type cannot ship without one.
func TestRegistryParamSchemaCoversEveryType(t *testing.T) {
	reg := NewRegistry()
	schemas := reg.AllSchemas()

	types := reg.Types()
	require.Len(t, schemas, len(types), "AllSchemas must return one entry per registered type")

	knownFieldTypes := []string{"string", "number", "boolean", "object", "select"}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			schema := reg.ParamSchema(typ)
			require.NotEmpty(t, schema, "the rule builder cannot render a form for %s without a schema", typ)
			require.Contains(t, schemas, typ, "AllSchemas is missing %s", typ)
			require.Equal(t, schema, schemas[typ], "AllSchemas and ParamSchema disagree for %s", typ)

			for _, f := range schema {
				require.NotEmpty(t, f.Name, "%s has a field with no name", typ)
				require.Contains(t, knownFieldTypes, f.Type, "%s.%s has unknown type %q", typ, f.Name, f.Type)
				if f.Type == "select" {
					require.NotEmpty(t, f.Options, "%s.%s is a select with no options", typ, f.Name)
					if f.Default != "" {
						require.Contains(t, f.Options, f.Default,
							"%s.%s defaults to %q, which is not one of its options", typ, f.Name, f.Default)
					}
				}
			}
		})
	}

	assert.Nil(t, reg.ParamSchema("not_a_rule"), "an unknown type must fall back to the JSON textarea")

	// A registered type missing from the mapping means the comparison test
	// below silently skips it, so the mapping has to cover the registry.
	require.ElementsMatch(t, types, keysOf(schemaParamsStructs),
		"schemaParamsStructs must name every registered type")
}

// TestParamSchemasStayInStepWithParamsStructs is the drift guard: a field
// added to a params struct but not to its schema cannot be set from the
// dashboard, and a field renamed in one and not the other produces a form
// whose input is silently ignored. Every mismatch in either direction fails
// here.
func TestParamSchemasStayInStepWithParamsStructs(t *testing.T) {
	reg := NewRegistry()

	for typ, sample := range schemaParamsStructs {
		t.Run(typ, func(t *testing.T) {
			schema := reg.ParamSchema(typ)
			require.NotEmpty(t, schema, "no schema to compare against")

			declared := make(map[string]bool, len(schema))
			for _, f := range schema {
				declared[f.Name] = true
			}

			structFields := jsonFieldNames(reflect.TypeOf(sample))

			for name := range declared {
				if schemaOnlyFields[name] {
					continue
				}
				assert.Contains(t, structFields, name,
					"schema declares %q but %s' params struct has no such field: the form input would be ignored", name, typ)
			}
			for name := range structFields {
				assert.Contains(t, declared, name,
					"params field %q has no schema entry: it cannot be set from the dashboard", name)
			}
		})
	}
}

// jsonFieldNames returns the JSON object keys a struct decodes into: the
// name from each json tag (or the field name when the tag is absent), with
// tag options like omitempty stripped.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = t.Field(i).Name
		}
		names[name] = true
	}
	return names
}

// TestRequiredSchemaFieldsAreEnforcedByValidate walks every schema field
// marked required, removes it from an otherwise valid params blob, and
// requires the registry to reject the result. A field the builder marks
// required that Validate lets through would let a user submit a rule the
// evaluator cannot run.
func TestRequiredSchemaFieldsAreEnforcedByValidate(t *testing.T) {
	reg := NewRegistry()

	for typ, schema := range reg.AllSchemas() {
		t.Run(typ, func(t *testing.T) {
			full := buildFullParams(t, reg, typ, schema)
			for _, f := range schema {
				if !f.Required {
					continue
				}
				require.Contains(t, full, f.Name,
					"the fixture never filled required field %s.%s; the rest of this test proves nothing", typ, f.Name)

				trimmed := make(map[string]any, len(full)-1)
				for k, v := range full {
					if k != f.Name {
						trimmed[k] = v
					}
				}
				raw, err := json.Marshal(trimmed)
				require.NoError(t, err)
				require.Error(t, reg.Validate(typ, raw),
					"%s.%s is marked required, but Validate accepts params without it", typ, f.Name)
			}
		})
	}
}

// buildFullParams produces an otherwise-valid params blob for typ: required
// fields and optionals alike come from the form fixtures and schema defaults,
// the same path the dashboard takes when the operator fills the form in.
func buildFullParams(t *testing.T, reg *Registry, typ string, schema []FieldSchema) map[string]any {
	t.Helper()
	values := make(map[string]string, len(schema))
	for _, f := range schema {
		if v, ok := formFixtures[typ][f.Name]; ok {
			values[f.Name] = v
			continue
		}
		if f.Default != "" {
			values[f.Name] = f.Default
			continue
		}
		if f.Required {
			t.Fatalf("no form fixture for required field %s.%s", typ, f.Name)
		}
	}
	raw, err := ParamsFromForm(schema, values)
	require.NoError(t, err)

	var params map[string]any
	require.NoError(t, json.Unmarshal(raw, &params))
	// The dashboard parses object fields (JSON arrays and maps typed into a
	// textarea) after ParamsFromForm, so mirror that step.
	for _, f := range schema {
		if f.Type != "object" || values[f.Name] == "" {
			continue
		}
		var obj any
		require.NoError(t, json.Unmarshal([]byte(values[f.Name]), &obj), "field %s", f.Name)
		params[f.Name] = obj
	}
	return params
}

// keysOf returns a map's keys, for the registry-vs-mapping check above.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
