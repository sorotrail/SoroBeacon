package stellar

import (
	"fmt"
	"io"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ContractSpec is a contract's exported interface, parsed from the
// contractspecv0 Wasm custom section (SEP-0048). Only the event entries are
// retained: they are what named event decoding needs.
//
// Contributors: the spec is looked up through SpecSource, so tests can supply
// a fixture instead of talking to an RPC node.
type ContractSpec struct {
	Events []*EventSpec
}

// EventSpec describes one event a contract publishes. Name is the spec's
// descriptive name (e.g. "Transfer"); PrefixTopics are the static leading
// topics emitted before the dynamic parameters (e.g. ["transfer"]). Matching
// uses the prefix topics because Name need not appear in the event at all.
type EventSpec struct {
	Name         string
	PrefixTopics []string
	Params       []EventParam
	DataFormat   xdr.ScSpecEventDataFormat
}

// EventParam is one named event parameter. Location says whether it is
// carried in the event's topic list or in its data value.
type EventParam struct {
	Name     string
	Location xdr.ScSpecEventParamLocationV0
}

// ParseContractSpec builds a ContractSpec from decoded spec entries. It
// returns nil when there are no event entries, which callers treat the same
// as a contract having no spec: fall back to default decoding.
func ParseContractSpec(entries []xdr.ScSpecEntry) *ContractSpec {
	spec := &ContractSpec{}
	for _, entry := range entries {
		event, ok := entry.GetEventV0()
		if !ok {
			// Function, struct, union and enum entries do not affect event
			// decoding; the event parameters carry enough type information.
			continue
		}
		es := &EventSpec{
			Name:       string(event.Name),
			DataFormat: event.DataFormat,
		}
		for _, prefix := range event.PrefixTopics {
			es.PrefixTopics = append(es.PrefixTopics, string(prefix))
		}
		for _, p := range event.Params {
			es.Params = append(es.Params, EventParam{Name: p.Name, Location: p.Location})
		}
		spec.Events = append(spec.Events, es)
	}
	if len(spec.Events) == 0 {
		return nil
	}
	return spec
}

// FillFields populates ev.Fields from the spec, mapping named parameters onto
// the event's topics and data. It reports whether the event matched an event
// spec exactly; on a mismatch ev is left untouched so the caller can keep the
// default, positional decoding.
//
// Matching requires the leading topics to equal an event's PrefixTopics, and
// the remaining topics to line up with the spec's topic parameters. The data
// value is unpacked according to the spec's data format. Anything unexpected
// — extra topics, a data shape that does not fit — is treated as "no match"
// rather than a partial decode, because a half-named event is worse for rule
// authors than the positional one they already had.
func (s *ContractSpec) FillFields(ev *DecodedEvent) bool {
	spec := s.matchEvent(ev.Topics)
	if spec == nil {
		return false
	}

	var topicParams, dataParams []EventParam
	for _, p := range spec.Params {
		if p.Location == xdr.ScSpecEventParamLocationV0ScSpecEventParamLocationTopicList {
			topicParams = append(topicParams, p)
		} else {
			dataParams = append(dataParams, p)
		}
	}
	if len(ev.Topics) != len(spec.PrefixTopics)+len(topicParams) {
		return false
	}

	fields := make(map[string]any, len(spec.Params))
	for i, p := range topicParams {
		fields[p.Name] = ev.Topics[len(spec.PrefixTopics)+i]
	}

	switch spec.DataFormat {
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatSingleValue:
		switch len(dataParams) {
		case 0:
			// No data parameters: the value is a void.
		case 1:
			fields[dataParams[0].Name] = ev.Value
		default:
			return false
		}
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatVec:
		values, ok := ev.Value.([]any)
		if !ok || len(values) != len(dataParams) {
			return false
		}
		for i, p := range dataParams {
			fields[p.Name] = values[i]
		}
	case xdr.ScSpecEventDataFormatScSpecEventDataFormatMap:
		values, ok := ev.Value.(map[string]any)
		if !ok {
			return false
		}
		for _, p := range dataParams {
			v, ok := values[p.Name]
			if !ok {
				return false
			}
			fields[p.Name] = v
		}
	default:
		return false
	}

	ev.Fields = fields
	return true
}

// matchEvent returns the spec whose prefix topics best match the event's
// leading topics. The longest prefix wins so that event families sharing a
// first topic still disambiguate (SEP-41, for example).
func (s *ContractSpec) matchEvent(topics []any) *EventSpec {
	var best *EventSpec
	for _, e := range s.Events {
		if !e.prefixMatches(topics) {
			continue
		}
		if best == nil || len(e.PrefixTopics) > len(best.PrefixTopics) {
			best = e
		}
	}
	return best
}

func (e *EventSpec) prefixMatches(topics []any) bool {
	if len(topics) < len(e.PrefixTopics) {
		return false
	}
	for i, want := range e.PrefixTopics {
		got, ok := topicText(topics[i])
		if !ok || got != want {
			return false
		}
	}
	return true
}

// topicText extracts the string a topic decodes to. Prefix topics are symbols,
// but SEP-0048 asks parsers to tolerate contracts that emitted them as strings,
// and the RPC's xdrFormat:"json" path decodes both to a bare string.
func topicText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case map[string]any:
		if s, ok := t["symbol"].(string); ok {
			return s, true
		}
		if s, ok := t["string"].(string); ok {
			return s, true
		}
	}
	return "", false
}

// specEntriesFromWasm extracts the contractspecv0 entries from a contract's
// Wasm. The spec lives in one Wasm custom section (SEP-0048) whose payload is
// a stream of XDR ScSpecEntry values with no framing. A Wasm with no spec
// section yields no entries and no error; callers turn that into ErrNoSpec.
func specEntriesFromWasm(wasm []byte) ([]xdr.ScSpecEntry, error) {
	const wasmMagic = "\x00asm"
	if len(wasm) < 8 || string(wasm[:4]) != wasmMagic {
		return nil, fmt.Errorf("contract code is not a Wasm module")
	}
	rest := wasm[8:] // skip magic and version
	dec := xdr.NewBytesDecoder()

	var entries []xdr.ScSpecEntry
	for len(rest) > 0 {
		sectionID := rest[0]
		rest = rest[1:]
		size, n, err := readULEB128(rest)
		if err != nil {
			return nil, fmt.Errorf("wasm section length: %w", err)
		}
		rest = rest[n:]
		if size > uint64(len(rest)) {
			return nil, fmt.Errorf("wasm section length %d exceeds remaining %d bytes", size, len(rest))
		}
		payload := rest[:size]
		rest = rest[size:]

		// Only custom sections (id 0) carry the spec.
		if sectionID != 0 {
			continue
		}
		nameLen, n, err := readULEB128(payload)
		if err != nil {
			return nil, fmt.Errorf("wasm custom section name length: %w", err)
		}
		if nameLen > uint64(len(payload)-n) {
			return nil, fmt.Errorf("wasm custom section name length %d exceeds remaining %d bytes", nameLen, len(payload)-n)
		}
		name := string(payload[n : n+int(nameLen)])
		content := payload[n+int(nameLen):]
		if name != "contractspecv0" {
			continue
		}
		for len(content) > 0 {
			var entry xdr.ScSpecEntry
			consumed, err := dec.DecodeBytes(&entry, content)
			if err != nil {
				return nil, fmt.Errorf("decode contract spec entry: %w", err)
			}
			if consumed <= 0 {
				return nil, fmt.Errorf("decode contract spec entry: made no progress")
			}
			entries = append(entries, entry)
			content = content[consumed:]
		}
	}
	return entries, nil
}

// readULEB128 decodes an unsigned LEB128 integer, the length encoding Wasm
// uses throughout. It returns the value and the number of bytes consumed.
func readULEB128(b []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		result |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, 0, fmt.Errorf("uleb128 value overflows 64 bits")
		}
	}
	return 0, 0, io.ErrUnexpectedEOF
}
