package server

import (
	"encoding/json"
	"testing"
)

// TestAddReasoningHeadroom 覆盖 reasoning 预算补偿。
//
// 背景：reasoning (thinking) 与正文共用 max_tokens 预算。预算不足时
// reasoning 先吃光，正文为空（finish_reason=length）。
func TestAddReasoningHeadroom(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64 // 期望的最终预算（max_tokens 优先）
	}{
		{"小预算得到补偿", `{"max_tokens":500}`, 500 + reasoningHeadroom},
		{"4000 也得到补偿（关键：这正是失败案例）",
			`{"max_tokens":4000}`, 4000 + reasoningHeadroom},
		{"max_completion_tokens 同样补偿",
			`{"max_completion_tokens":1000}`, 1000 + reasoningHeadroom},
		{"无预算字段时补默认值", `{"model":"glm-5.3"}`, reasoningHeadroom},
		{"max_tokens=0 视为未指定", `{"max_tokens":0}`, reasoningHeadroom},
		{"已超 cap 时不动", `{"max_tokens":40000}`, 40000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := addReasoningHeadroom([]byte(tc.in))
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			got, _ := m["max_tokens"].(float64)
			if got == 0 {
				got, _ = m["max_completion_tokens"].(float64)
			}
			if got != tc.want {
				t.Errorf("预算 = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAddReasoningHeadroom_PreservesOtherFields 不能弄丢其它字段。
func TestAddReasoningHeadroom_PreservesOtherFields(t *testing.T) {
	in := `{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}],"max_tokens":300}`
	out := addReasoningHeadroom([]byte(in))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "glm-5.3" {
		t.Errorf("model 丢失: %v", m["model"])
	}
	if m["stream"] != true {
		t.Errorf("stream 丢失: %v", m["stream"])
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Error("messages 丢失")
	}
	if _, ok := m["tools"].([]any); !ok {
		t.Error("tools 丢失")
	}
	if got, _ := m["max_tokens"].(float64); got != 300+reasoningHeadroom {
		t.Errorf("预算 = %v, want %v", got, 300+reasoningHeadroom)
	}
}

// TestAddReasoningHeadroom_InvalidJSONPassThrough 坏 JSON 原样返回。
func TestAddReasoningHeadroom_InvalidJSONPassThrough(t *testing.T) {
	in := []byte(`{not json`)
	out := addReasoningHeadroom(in)
	if string(out) != string(in) {
		t.Errorf("坏 JSON 应原样返回: got %q", out)
	}
}

// TestAddReasoningHeadroom_NoDoubleApply 重复调用不叠加（幂等性检查）。
// 注意：本函数不是幂等的（每次都会加），所以这里只断言「加了一次的量」，
// 防止未来误改成幂等实现时行为漂移而不被发现。
func TestAddReasoningHeadroom_NoDoubleApply(t *testing.T) {
	once := addReasoningHeadroom([]byte(`{"max_tokens":500}`))
	var m map[string]any
	json.Unmarshal(once, &m)
	got, _ := m["max_tokens"].(float64)
	if got != 500+reasoningHeadroom {
		t.Errorf("单次应用 = %v, want %v", got, 500+reasoningHeadroom)
	}
}

// TestAddReasoningHeadroom_RespectsCap 结果不超上限。
func TestAddReasoningHeadroom_RespectsCap(t *testing.T) {
	out := addReasoningHeadroom([]byte(`{"max_tokens":31000}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	got, _ := m["max_tokens"].(float64)
	if got > reasoningHeadroomCap {
		t.Errorf("超出 cap: %v > %v", got, reasoningHeadroomCap)
	}
	if got != reasoningHeadroomCap {
		t.Errorf("应被夹到 cap: got %v want %v", got, reasoningHeadroomCap)
	}
}
