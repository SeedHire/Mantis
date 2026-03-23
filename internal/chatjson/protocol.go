// Package chatjson implements a JSON Lines protocol for the VSCode extension
// to communicate with the Mantis AI chat over stdin/stdout.
package chatjson

// Request is a JSON-RPC-lite inbound message from the extension.
type Request struct {
	ID     int    `json:"id"`
	Method string `json:"method"` // "chat", "command", "cancel"
	Params Params `json:"params"`
}

// Params holds the union of all method parameters.
type Params struct {
	// chat
	Message string `json:"message,omitempty"`
	// command
	Name string `json:"name,omitempty"`
	Args string `json:"args,omitempty"`
}

// Response is a single outbound JSON line.
type Response struct {
	ID      int    `json:"id"`
	Type    string `json:"type"`              // "token", "done", "file_write", "error", "status", "routing"
	Text    string `json:"text,omitempty"`     // token content or status message
	Model   string `json:"model,omitempty"`    // model used
	Tier    string `json:"tier,omitempty"`     // routing tier
	Path    string `json:"path,omitempty"`     // file_write: file path
	Preview string `json:"preview,omitempty"`  // file_write: first N lines
	Tokens  int    `json:"tokens,omitempty"`   // done: total tokens
	Error   string `json:"error,omitempty"`    // error message
}
