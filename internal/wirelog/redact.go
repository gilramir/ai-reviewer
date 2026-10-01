package wirelog

import "regexp"

// The stream-json protocol itself carries no credential. The CLI authenticates
// on its own, and the closest the init frame comes is apiKeySource, which names
// where a key came from ("none", "ANTHROPIC_API_KEY") and never holds one; the
// environment, where a key would live, is not logged at all.
//
// What can carry one is content. A Read of a .env file puts the file in a
// tool_result, and a prompt can quote whatever the reviewer pasted. So the log
// scrubs by shape rather than by field, on every frame, both ways, and on
// stderr: anything that looks like a provider's key, and the string value of
// any JSON member whose name says it is a secret.
var (
	// Anthropic (sk-ant-...), OpenAI (sk-..., sk-proj-...), and bearer tokens
	// written out in a header.
	keyShaped = regexp.MustCompile(`sk-(?:ant-|proj-)?[A-Za-z0-9_\-]{20,}|(?i:bearer)\s+[A-Za-z0-9_\-.=]{20,}`)

	// A JSON member named like a credential, whose value is a string. Names
	// are matched whole, so input_tokens and apiKeySource stay readable. The
	// value pattern allows for escaped quotes inside it.
	secretMember = regexp.MustCompile(`("(?i:api[_-]?key|x-api-key|[a-z_]*access[_-]?token|refresh[_-]?token|auth[_-]?token|token|secret|client[_-]?secret|password|authorization)"\s*:\s*)"(?:[^"\\]|\\.)*"`)
)

const redacted = "[redacted]"

// redact returns b with anything that looks like a credential replaced. It
// edits the text rather than decoding and re-encoding it, which would reorder
// every object's keys and lose the frame as the CLI wrote it.
func redact(b []byte) []byte {
	b = secretMember.ReplaceAll(b, []byte(`$1"`+redacted+`"`))
	return keyShaped.ReplaceAll(b, []byte(redacted))
}
