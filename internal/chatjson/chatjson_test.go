package chatjson

import (
	"encoding/json"
	"testing"
)

func TestRequestUnmarshal(t *testing.T) {
	input := `{"id":1,"method":"chat","params":{"message":"hello world"}}`
	var req Request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != 1 {
		t.Errorf("ID = %d, want 1", req.ID)
	}
	if req.Method != "chat" {
		t.Errorf("Method = %q, want %q", req.Method, "chat")
	}
	if req.Params.Message != "hello world" {
		t.Errorf("Message = %q, want %q", req.Params.Message, "hello world")
	}
}

func TestRequestCommand(t *testing.T) {
	input := `{"id":2,"method":"command","params":{"name":"reset"}}`
	var req Request
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "command" {
		t.Errorf("Method = %q, want %q", req.Method, "command")
	}
	if req.Params.Name != "reset" {
		t.Errorf("Name = %q, want %q", req.Params.Name, "reset")
	}
}

func TestResponseMarshal(t *testing.T) {
	resp := Response{
		ID:    1,
		Type:  "routing",
		Tier:  "code",
		Model: "qwen2.5-coder:32b",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "routing" {
		t.Errorf("type = %v, want routing", m["type"])
	}
	if m["tier"] != "code" {
		t.Errorf("tier = %v, want code", m["tier"])
	}
	// Ensure omitempty works: text should not be present.
	if _, ok := m["text"]; ok {
		t.Error("text should be omitted when empty")
	}
}

func TestResponseTokenType(t *testing.T) {
	resp := Response{ID: 1, Type: "token", Text: "Hello"}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["type"] != "token" {
		t.Errorf("type = %v, want token", m["type"])
	}
	if m["text"] != "Hello" {
		t.Errorf("text = %v, want Hello", m["text"])
	}
}

func TestResponseDoneType(t *testing.T) {
	resp := Response{ID: 1, Type: "done", Tokens: 150, Model: "qwen2.5-coder:7b", Tier: "fast"}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if m["type"] != "done" {
		t.Errorf("type = %v, want done", m["type"])
	}
	if int(m["tokens"].(float64)) != 150 {
		t.Errorf("tokens = %v, want 150", m["tokens"])
	}
}
