package redact

import (
	"encoding/json"
	"fmt"
)

const ManifestFile = "redaction-inputs.json"

// Manifest names secret sources without persisting their values in the
// sandbox control directory. CredentialFile is an in-container read-only path.
type Manifest struct {
	EnvNames       []string `json:"env_names,omitempty"`
	CredentialFile string   `json:"credential_file,omitempty"`
}

// JSONStringValues extracts all string leaves from a credential document.
// Over-redacting identifiers is preferable to omitting a nested OAuth token.
func JSONStringValues(data []byte) ([]string, error) {
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse credential JSON for redaction: %w", err)
	}
	var values []string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				values = append(values, typed)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(document)
	return values, nil
}
