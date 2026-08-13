package policy

import "encoding/json"

// jsonMarshal exists so Policy.MarshalJSON can serialise its alias struct
// without importing encoding/json into policy.go and shadowing the method it
// is implementing.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
