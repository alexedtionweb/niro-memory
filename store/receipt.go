package store

import (
	"bytes"
	"encoding/json"
	"strings"
)

// EncodeReceipt preserves result bytes even on databases that canonicalize JSON.
func EncodeReceipt(r *Receipt) ([]byte, error) {
	return json.Marshal(struct {
		*Receipt
		Result []byte
	}{Receipt: r, Result: r.Result})
}

// DecodeReceipt restores a receipt written with EncodeReceipt.
func DecodeReceipt(raw []byte) (*Receipt, error) {
	r := &Receipt{}
	v := struct {
		*Receipt
		Result []byte
	}{Receipt: r}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	r.Result = json.RawMessage(v.Result)
	return r, nil
}

// RedactReceipt clears a result mentioning an erased memory, retaining the
// operation hash and key tombstone so a retry cannot recreate private data.
func RedactReceipt(r *Receipt, deleted []string) bool {
	var decoded any
	_ = json.Unmarshal(r.Result, &decoded)
	var mentions func(any, string) bool
	mentions = func(v any, id string) bool {
		switch v := v.(type) {
		case string:
			return strings.Contains(v, id)
		case []any:
			for _, item := range v {
				if mentions(item, id) {
					return true
				}
			}
		case map[string]any:
			for key, item := range v {
				if strings.Contains(key, id) || mentions(item, id) {
					return true
				}
			}
		}
		return false
	}
	for _, id := range deleted {
		if id == "" {
			continue
		}
		quoted, _ := json.Marshal(id)
		if bytes.Contains(r.Result, []byte(id)) || bytes.Contains(r.Result, quoted[1:len(quoted)-1]) || mentions(decoded, id) {
			r.Result = nil
			return true
		}
	}
	return false
}
