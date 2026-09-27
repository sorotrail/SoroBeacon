package stellar

import (
	"github.com/stellar/go-stellar-sdk/xdr"
)

// TopicWildcardMany matches any number of trailing topics. The RPC's topic
// filters match positionally, with "*" matching exactly one segment and "**"
// matching the rest, so a filter for a named event is
// [<symbol>, TopicWildcardMany].
const TopicWildcardMany = "**"

// SymbolTopicScVal returns the base64-encoded ScVal a getEvents topic filter
// uses to match a topic holding the symbol `name`. Filter segments are encoded
// ScVals, not plain strings — the RPC compares them as bytes, so a plain
// "transfer" matches nothing while the encoded form matches every transfer
// event. The encoding is the ScSymbol XDR: the ScVal type discriminant, the
// string length, then the symbol bytes.
func SymbolTopicScVal(name string) (string, error) {
	sym := xdr.ScSymbol(name)
	return xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})
}

// SymbolTopicFilter builds the topic-filter entry for one event name: the
// encoded symbol followed by a trailing wildcard, so it matches events whose
// first topic is the name regardless of how many topics follow.
func SymbolTopicFilter(name string) ([]string, error) {
	encoded, err := SymbolTopicScVal(name)
	if err != nil {
		return nil, err
	}
	return []string{encoded, TopicWildcardMany}, nil
}
