package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestCleanSSEDataLine_RemovesReasoning 覆盖流式 reasoning_content 过滤。
func TestCleanSSEDataLine_RemovesReasoning(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantReason  bool // output masih mengandung reasoning_content?
		wantContent string
	}{
		{
			name:        "delta dengan reasoning saja",
			in:          `data: {"choices":[{"delta":{"reasoning_content":"thinking..."},"index":0}]}` + "\n",
			wantReason:  false,
			wantContent: "",
		},
		{
			name:        "delta dengan reasoning + content",
			in:          `data: {"choices":[{"delta":{"content":"Hello","reasoning_content":"hmm"},"index":0}]}` + "\n",
			wantReason:  false,
			wantContent: "Hello",
		},
		{
			name:        "delta tanpa reasoning (diteruskan apa adanya)",
			in:          `data: {"choices":[{"delta":{"content":"world"},"index":0}]}` + "\n",
			wantReason:  false,
			wantContent: "world",
		},
		{
			name:        "message.reasoning_content (非流式形态混入)",
			in:          `data: {"choices":[{"message":{"content":"hi","reasoning_content":"x"}}]}` + "\n",
			wantReason:  false,
			wantContent: "hi",
		},
		{
			name:        "[DONE] 原样保留",
			in:          "data: [DONE]\n",
			wantReason:  false,
			wantContent: "[DONE]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := cleanSSEDataLine([]byte(tc.in))
			got := string(out)
			if strings.Contains(got, "reasoning_content") {
				t.Errorf("reasoning_content 未被移除: %q", got)
			}
			if !strings.HasPrefix(got, "data:") {
				t.Errorf("前缀丢失: %q", got)
			}
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("换行丢失（会破坏 SSE framing）: %q", got)
			}
			if tc.wantContent != "" && !strings.Contains(got, tc.wantContent) {
				t.Errorf("内容丢失: want %q in %q", tc.wantContent, got)
			}
		})
	}
}

// TestCleanSSEDataLine_PreservesNonDataLines 非 data: 行必须原样通过。
func TestCleanSSEDataLine_PreservesNonDataLines(t *testing.T) {
	lines := []string{
		"\n",
		"event: ping\n",
		": comment\n",
		"id: 42\n",
		"retry: 1000\n",
	}
	for _, ln := range lines {
		out := string(cleanSSEDataLine([]byte(ln)))
		if out != ln {
			t.Errorf("非 data 行被改动: in=%q out=%q", ln, out)
		}
	}
}

// TestCleanSSEDataLine_InvalidJSONPassThrough 坏 JSON 不能丢数据。
func TestCleanSSEDataLine_InvalidJSONPassThrough(t *testing.T) {
	in := "data: {not valid json\n"
	out := string(cleanSSEDataLine([]byte(in)))
	if out != in {
		t.Errorf("非法 JSON 应原样通过: in=%q out=%q", in, out)
	}
}

// TestCleanSSEDataLine_MultipleChoices 多 choice 都要清。
func TestCleanSSEDataLine_MultipleChoices(t *testing.T) {
	in := `data: {"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b","content":"c"}}]}` + "\n"
	out := string(cleanSSEDataLine([]byte(in)))
	if strings.Contains(out, "reasoning_content") {
		t.Errorf("多 choice 未全部清理: %q", out)
	}
	if !strings.Contains(out, `"content":"c"`) {
		t.Errorf("合法内容丢失: %q", out)
	}
}

// TestStreamFiltered_EndToEnd 模拟整条 SSE 流。
func TestStreamFiltered_EndToEnd(t *testing.T) {
	var src strings.Builder
	src.WriteString(": keepalive\n\n")
	src.WriteString(`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n")
	src.WriteString(`data: {"choices":[{"delta":{"reasoning_content":"let me think"}}]}` + "\n\n")
	src.WriteString(`data: {"choices":[{"delta":{"content":"4"}}]}` + "\n\n")
	src.WriteString(`data: {"choices":[{"delta":{"reasoning_content":"more"}}]}` + "\n\n")
	src.WriteString(`data: {"choices":[{"delta":{"content":"2"}}]}` + "\n\n")
	src.WriteString("data: [DONE]\n\n")

	var out bytes.Buffer
	if err := streamFiltered(&out, strings.NewReader(src.String()), nil); err != nil {
		t.Fatalf("streamFiltered error: %v", err)
	}
	got := out.String()

	if strings.Contains(got, "reasoning_content") {
		t.Errorf("流里仍有 reasoning_content:\n%s", got)
	}
	if strings.Contains(got, "let me think") || strings.Contains(got, "more") {
		t.Errorf("reasoning 内容泄漏:\n%s", got)
	}
	// 正文与 [DONE] 必须保留
	if !strings.Contains(got, `"content":"4"`) || !strings.Contains(got, `"content":"2"`) {
		t.Errorf("正文丢失:\n%s", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Errorf("[DONE] 丢失:\n%s", got)
	}
	if !strings.Contains(got, ": keepalive") {
		t.Errorf("注释行丢失:\n%s", got)
	}

	// 每一行 data: 都应是合法 JSON 或 [DONE]
	for _, ln := range strings.Split(got, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data:") {
			continue
		}
		p := strings.TrimSpace(ln[len("data:"):])
		if p == "[DONE]" || p == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(p), &v); err != nil {
			t.Errorf("输出行不是合法 JSON: %q (%v)", p, err)
		}
	}
}

// TestStreamFiltered_PreservesByteStreamForPlainContent 纯文本流应内容等价。
func TestStreamFiltered_PreservesByteStreamForPlainContent(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"abc"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := streamFiltered(&out, strings.NewReader(src), nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != src {
		t.Errorf("无 reasoning 的流被改动:\nin =%q\nout=%q", src, out.String())
	}
}

// TestStreamFiltered_ReadErrorPropagates 读错误要向上传。
func TestStreamFiltered_ReadErrorPropagates(t *testing.T) {
	err := streamFiltered(io.Discard, &failingReader{}, nil)
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		f.n++
		copy(p, "data: {\"choices\":[]}\n")
		return len("data: {\"choices\":[]}\n"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
