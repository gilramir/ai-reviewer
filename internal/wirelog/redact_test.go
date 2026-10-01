package wirelog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"an Anthropic key read out of a .env file",
			`{"content":"ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123\nDEBUG=1"}`,
			`{"content":"ANTHROPIC_API_KEY=[redacted]\nDEBUG=1"}`,
		},
		{
			"an OpenAI project key",
			`export OPENAI_API_KEY=sk-proj-AbCdEfGhIjKlMnOpQrStUvWx_yz`,
			`export OPENAI_API_KEY=[redacted]`,
		},
		{
			"a bearer token in a header",
			`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`,
			`Authorization: [redacted]`,
		},
		{
			"a member named like a credential, with an escaped quote in its value",
			`{"api_key":"abc\"def","password": "hunter2","n":1}`,
			`{"api_key":"[redacted]","password": "[redacted]","n":1}`,
		},
		{
			"token counts and the name of where a key came from stay readable",
			`{"apiKeySource":"ANTHROPIC_API_KEY","usage":{"input_tokens":12,"output_tokens":3}}`,
			`{"apiKeySource":"ANTHROPIC_API_KEY","usage":{"input_tokens":12,"output_tokens":3}}`,
		},
		{
			"ordinary prose that mentions keys",
			`{"text":"the sk- prefix marks a secret key"}`,
			`{"text":"the sk- prefix marks a secret key"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, string(redact([]byte(c.in))))
		})
	}
}
