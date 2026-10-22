package mcpserver

import "encoding/json"

// maxAnswer is the most one tool result may take, JSON-encoded. Claude Code cuts MCP tool output at
// about 25k tokens by default, and 80 KiB of text is about 20k.
const maxAnswer = 80 << 10

// answerOverhead is room for what a result carries beside its content: the JSON-RPC envelope, field
// names and short lines of text.
const answerOverhead = 1 << 10

// encodedSize is how many bytes v takes JSON-encoded, or 0 if it can't be.
func encodedSize(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(raw)
}
