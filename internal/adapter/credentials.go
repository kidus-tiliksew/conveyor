package adapter

// CredentialLayout is the adapter-owned contract between the trusted runner
// and the in-container shim. Harnesses may alternatively authenticate only
// through explicitly injected environment variables.
type CredentialLayout struct {
	FileName string
}

var credentialLayouts = map[string]CredentialLayout{
	"codex":       {FileName: "auth.json"},
	"claude-code": {FileName: ".credentials.json"},
}

func CredentialLayoutFor(harness string) (CredentialLayout, bool) {
	layout, ok := credentialLayouts[harness]
	return layout, ok
}
