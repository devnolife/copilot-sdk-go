package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestDecodeArgs(t *testing.T) {
	type payload struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	cases := []struct {
		name string
		in   any
		want map[string]any
	}{
		{"nil ke map kosong", nil, map[string]any{}},
		{"map apa adanya", map[string]any{"q": "x"}, map[string]any{"q": "x"}},
		{"string JSON diparse", `{"q":"x","n":2}`, map[string]any{"q": "x", "n": float64(2)}},
		{"string bukan JSON disimpan di _raw", "bukan json", map[string]any{"_raw": "bukan json"}},
		{"struct dimarshal ulang", payload{Query: "y", Limit: 3}, map[string]any{"query": "y", "limit": float64(3)}},
		{"bukan objek ke map kosong", []int{1, 2}, map[string]any{}},
		{"channel tidak bisa dimarshal ke map kosong", make(chan int), map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decodeArgs(%v) = %v, ingin %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCallLogSnapshotIsCopy(t *testing.T) {
	log := &callLog{}
	if got := log.snapshot(); len(got) != 0 {
		t.Fatalf("snapshot log kosong = %v", got)
	}
	log.add(ToolCall{Tool: "a", Result: "1"})
	snap := log.snapshot()
	snap[0].Result = "diubah"
	if log.items[0].Result != "1" {
		t.Fatal("snapshot harus salinan, bukan alias ke slice internal")
	}
	log.add(ToolCall{Tool: "b"})
	if len(snap) != 1 || len(log.snapshot()) != 2 {
		t.Fatalf("snapshot lama ikut berubah: %v", snap)
	}
}

func TestBuildToolsNoopWithoutSpecsOrExecutor(t *testing.T) {
	emit := func(Event) {}
	exec := func(context.Context, string, map[string]any) (string, error) { return "", nil }

	if tools, log := buildTools(context.Background(), nil, exec, emit); tools != nil || log == nil {
		t.Errorf("tanpa spec: tools=%v log=%v", tools, log)
	}
	specs := []ToolSpec{{Name: "x"}}
	if tools, log := buildTools(context.Background(), specs, nil, emit); tools != nil || log == nil {
		t.Errorf("tanpa executor: tools=%v log=%v", tools, log)
	}
}

func TestBuildToolsHandlerSuccess(t *testing.T) {
	var events []Event
	emit := func(ev Event) { events = append(events, ev) }

	var gotName string
	var gotArgs map[string]any
	exec := func(_ context.Context, name string, args map[string]any) (string, error) {
		gotName, gotArgs = name, args
		return "3 paper ditemukan", nil
	}

	specs := []ToolSpec{{
		Name:        "cari_paper",
		Description: "Cari paper",
		Parameters:  map[string]any{"type": "object"},
	}}
	tools, log := buildTools(context.Background(), specs, exec, emit)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, ingin 1", len(tools))
	}
	tool := tools[0]
	if tool.Name != "cari_paper" || tool.Description != "Cari paper" || !tool.SkipPermission {
		t.Errorf("deklarasi tool tidak diteruskan utuh: %+v", tool)
	}
	if !reflect.DeepEqual(tool.Parameters, specs[0].Parameters) {
		t.Errorf("Parameters = %v", tool.Parameters)
	}

	res, err := tool.Handler(copilot.ToolInvocation{ToolName: "cari_paper", Arguments: `{"q":"fuzzy"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultType != "success" || res.TextResultForLLM != "3 paper ditemukan" || res.Error != "" {
		t.Errorf("ToolResult = %+v", res)
	}
	if gotName != "cari_paper" || !reflect.DeepEqual(gotArgs, map[string]any{"q": "fuzzy"}) {
		t.Errorf("executor menerima name=%q args=%v", gotName, gotArgs)
	}

	calls := log.snapshot()
	if len(calls) != 1 || calls[0].Tool != "cari_paper" || calls[0].Result != "3 paper ditemukan" {
		t.Errorf("jejak pemanggilan = %+v", calls)
	}
	if len(events) != 2 || events[0].Status != "start" || events[1].Status != "done" {
		t.Fatalf("events = %+v, ingin start lalu done", events)
	}
	if events[0].Type != "tool" || events[0].Name != "cari_paper" || !reflect.DeepEqual(events[0].Arguments, gotArgs) {
		t.Errorf("event start tidak membawa nama+argumen: %+v", events[0])
	}
}

func TestBuildToolsHandlerFailureIsReturnedToModel(t *testing.T) {
	var events []Event
	emit := func(ev Event) { events = append(events, ev) }
	exec := func(context.Context, string, map[string]any) (string, error) {
		return "", errors.New("database mati")
	}

	tools, log := buildTools(context.Background(), []ToolSpec{{Name: "baca"}}, exec, emit)
	res, err := tools[0].Handler(copilot.ToolInvocation{Arguments: nil})
	if err != nil {
		t.Fatalf("kegagalan tool harus dikembalikan sebagai hasil, bukan error transport: %v", err)
	}
	if res.ResultType != "error" || res.Error != "database mati" {
		t.Errorf("ToolResult = %+v", res)
	}
	if !strings.Contains(res.TextResultForLLM, "baca") || !strings.Contains(res.TextResultForLLM, "database mati") {
		t.Errorf("pesan untuk model harus menyebut tool dan sebabnya: %q", res.TextResultForLLM)
	}
	if len(log.snapshot()) != 0 {
		t.Error("pemanggilan gagal tidak boleh masuk jejak sukses")
	}
	if len(events) != 2 || events[1].Status != "done" {
		t.Errorf("events = %+v", events)
	}
}
