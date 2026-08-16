package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
)

// ToolCall mencatat satu pemanggilan tool beserta hasilnya, untuk audit trail
// yang dikembalikan ke pemanggil.
type ToolCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Result    string         `json:"result"`
}

// callLog aman dipakai dari goroutine milik runtime SDK.
type callLog struct {
	mu    sync.Mutex
	items []ToolCall
}

func (c *callLog) add(item ToolCall) {
	c.mu.Lock()
	c.items = append(c.items, item)
	c.mu.Unlock()
}

func (c *callLog) snapshot() []ToolCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ToolCall, len(c.items))
	copy(out, c.items)
	return out
}

// buildTools menjembatani deklarasi tool milik pemanggil ke tool SDK.
//
// Eksekusi sebenarnya terjadi di luar proses ini (backend Python), lewat
// callback “exec“. Setiap pemanggilan juga dipancarkan sebagai event supaya
// klien bisa menampilkan progres.
func buildTools(
	ctx context.Context,
	specs []ToolSpec,
	exec ToolExecutor,
	emit func(Event),
) ([]copilot.Tool, *callLog) {
	log := &callLog{}
	if len(specs) == 0 || exec == nil {
		return nil, log
	}

	tools := make([]copilot.Tool, 0, len(specs))
	for _, spec := range specs {
		spec := spec
		tools = append(tools, copilot.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  spec.Parameters,
			// Aman: hanya tool milik pemanggil yang terdaftar di sesi ini.
			SkipPermission: true,
			Handler: func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
				args := decodeArgs(inv.Arguments)
				emit(Event{Type: "tool", Status: "start", Name: spec.Name, Arguments: args})

				result, err := exec(ctx, spec.Name, args)
				if err != nil {
					// Kembalikan sebagai hasil, bukan error transport, supaya
					// model bisa memperbaiki langkahnya sendiri.
					msg := fmt.Sprintf("Tool %s gagal: %v", spec.Name, err)
					emit(Event{Type: "tool", Status: "done", Name: spec.Name})
					return copilot.ToolResult{
						TextResultForLLM: msg,
						ResultType:       "error",
						Error:            err.Error(),
					}, nil
				}

				log.add(ToolCall{Tool: spec.Name, Arguments: args, Result: result})
				emit(Event{Type: "tool", Status: "done", Name: spec.Name})
				return copilot.ToolResult{
					TextResultForLLM: result,
					ResultType:       "success",
				}, nil
			},
		})
	}
	return tools, log
}

// decodeArgs menormalkan argumen tool ke map. Runtime bisa mengirimkan map,
// string JSON, atau nil tergantung model.
func decodeArgs(raw any) map[string]any {
	switch v := raw.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	case string:
		out := map[string]any{}
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			return map[string]any{"_raw": v}
		}
		return out
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return map[string]any{}
		}
		out := map[string]any{}
		if err := json.Unmarshal(data, &out); err != nil {
			return map[string]any{}
		}
		return out
	}
}
