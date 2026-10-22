// Package canon produces canonical JSON and content hashes, so approvals and
// audit entries can be bound to the exact bytes the owner saw.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// JSON returns v as canonical JSON: object keys sorted, no insignificant
// whitespace, numbers preserved as written.
func JSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	// encoding/json writes map keys in sorted order.
	return json.Marshal(generic)
}

// Hash returns the hex sha256 of v's canonical JSON.
func Hash(v any) (string, error) {
	b, err := JSON(v)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// HashBytes returns the hex sha256 of b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
