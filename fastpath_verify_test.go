package main

// 快路径（streamfast.go）的验证：
//   1) 与 cleanChunkJSON 的差分（固定语料 + 随机语料 + Go 原生 fuzz）
//   2) 端到端：streamChatResponse 在 fastPathOn / fastPathOff 下的输出逐事件语义比对
//   3) 元数据（usage / finish_reason / 首 token）与旧实现一致

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- 1. 差分 ----------

var fastCorpus = []string{
	`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`,
	`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"I will check the git status."},"finish_reason":"","logprobs":null}]}`,
	`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":"","logprobs":null}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls"}]}`,
	`{"choices":[{"index":0,"delta":{"function_call":{"name":"get_weather","arguments":"{}"}},"finish_reason":"function_call"}]}`,
	`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"思考中"},"finish_reason":""}]}`,
	`{"choices":[]}`,
	`{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
	`{"choices":[{"index":0,"delta":{"content":"a\"b\\c"},"finish_reason":""}],"usage":{"prompt_tokens":1}}`,
	`{"choices":[{"index":0,"delta":{"content":"x","tool_calls":[]},"finish_reason":""}],"extra":{"a":[1,2,{"b":null}]}}`,
	`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":""},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"content":"  "},"finish_reason":""}]}`,
	`{"a":1,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":""}],"z":"末"}`,
	`{"choices":[{"index":0,"delta":{"content":"é\u00e9\ud83d\ude00"},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":6,"total_tokens":15}}`,
	`{ "choices" : [ { "index" : 0 , "delta" : { "content" : "x" } , "finish_reason" : "" } ] }`,
	`{"choices":[{"finish_reason":"","delta":{}},{"finish_reason":"","delta":{"content":"y"}}]}`,
	`{"choices":[{"index":0,"delta":{"reasoning":"r","content":"c"},"finish_reason":""}]}`,
	`{"id":"x","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":""}],"usage":null}`,
	`{"id":"x","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":""}],"usage":"nope"}`,
	`{"id":"x","choices":null}`,
	`{"id":"x","choices":{"a":1}}`,
	`{"id":"x"}`,
	`{}`,
	`null`,
	`[]`,
	`"str"`,
	`123`,
	`not json at all`,
	`event: ping`,
	``,
	`{"choices":[{"index":0,"delta":{"content":"未闭合"}`,
	`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"","logprobs":{"a":[1,{"b":"}"}]}}]}`,
	`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"","extra":"}"}]}`,
	`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"","extra":"\\"}]}`,
	`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"","n":12345678901234567890}]}`,
	`{"choices":[{"index":0,"delta":{"content":"x","content":"y"},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":""}}]},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"function_call":{}},"finish_reason":""}]}`,
	`{"choices":[{"index":0,"delta":{"function_call":"x"},"finish_reason":""}]}`,
}

// fastResult 复刻调用方的真实行为：快路径不可识别时回退到 cleanChunkJSON。
func fastResult(in string) string {
	buf, ok := cleanChunkInto(nil, []byte(in), &chunkMeta{})
	if !ok {
		return cleanChunkJSON(in)
	}
	return string(buf)
}

// equivalentJSON：两侧都能解析时按语义比；否则按字节比。
func equivalentJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	return reflect.DeepEqual(x, y)
}

// TestFastKnownDivergence 记录两处"只影响非法/病态输入"的差异，避免以后误以为
// 快路径与原实现处处逐字节一致。
//  1. encoding/json 判定为非法的输入（如 1e400 溢出 float64）：原实现 Unmarshal
//     失败后原样透传，快路径仍会改写 finish_reason。这类输入客户端本来也无法解析。
//  2. 同一对象里的重复键：原实现经 map 往返会合并（后者胜出），快路径原样保留。
func TestFastKnownDivergence(t *testing.T) {
	overflow := `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"","n":1e400}]}`
	if cleanChunkJSON(overflow) != overflow {
		t.Fatalf("前提变了：原实现现在能解析 1e400")
	}
	if got := fastResult(overflow); got == overflow {
		t.Log("注意：快路径也原样透传了（行为更保守）")
	}
	dup := `{"choices":[{"index":0,"delta":{"content":"x","content":"y"},"finish_reason":""}]}`
	var probe any
	if err := json.Unmarshal([]byte(fastResult(dup)), &probe); err != nil {
		t.Fatalf("重复键产出非法 JSON: %v", err)
	}
}

func TestFastCleanDifferentialCorpus(t *testing.T) {
	for i, in := range fastCorpus {
		want := cleanChunkJSON(in)
		got := fastResult(in)
		if !equivalentJSON(want, got) {
			t.Fatalf("case %d 不一致\n in : %s\nwant: %s\n got: %s", i, in, want, got)
		}
	}
}

// randChunk 生成结构随机的分片，覆盖转义、嵌套、空值、重复键、数字、Unicode。
func randChunk(r *rand.Rand) string {
	var b strings.Builder
	b.WriteString(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[`)
	nChoice := r.Intn(3)
	for c := 0; c < nChoice; c++ {
		if c > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"index":`)
		b.WriteString(fmt.Sprint(r.Intn(3)))
		if r.Intn(4) > 0 {
			b.WriteString(`,"delta":{`)
			nField := r.Intn(5)
			first := true
			for f := 0; f < nField; f++ {
				if !first {
					b.WriteByte(',')
				}
				first = false
				switch r.Intn(9) {
				case 0:
					b.WriteString(`"content":"`)
					b.WriteString(randText(r))
					b.WriteString(`"`)
				case 1:
					b.WriteString(`"reasoning_content":"`)
					b.WriteString(randText(r))
					b.WriteString(`"`)
				case 2:
					b.WriteString(`"content":""`)
				case 3:
					b.WriteString(`"tool_calls":[]`)
				case 4:
					b.WriteString(`"tool_calls":[{"index":0,"function":{"name":"`)
					b.WriteString(randText(r))
					b.WriteString(`","arguments":"`)
					b.WriteString(randText(r))
					b.WriteString(`"}}]`)
				case 5:
					b.WriteString(`"function_call":{"name":"","arguments":""}`)
				case 6:
					b.WriteString(`"role":"assistant"`)
				case 7:
					b.WriteString(`"content":null`)
				case 8:
					b.WriteString(`"x":`)
					b.WriteString(randScalar(r))
				}
			}
			b.WriteByte('}')
		}
		if r.Intn(3) > 0 {
			b.WriteString(`,"finish_reason":`)
			switch r.Intn(5) {
			case 0:
				b.WriteString(`""`)
			case 1:
				b.WriteString(`"stop"`)
			case 2:
				b.WriteString(`"tool_calls"`)
			case 3:
				b.WriteString(`null`)
			default:
				b.WriteString(`"length"`)
			}
		}
		if r.Intn(5) == 0 {
			b.WriteString(`,"logprobs":null`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	if r.Intn(3) == 0 {
		b.WriteString(`,"usage":{"prompt_tokens":`)
		b.WriteString(fmt.Sprint(r.Intn(1000)))
		b.WriteString(`,"completion_tokens":`)
		b.WriteString(fmt.Sprint(r.Intn(1000)))
		b.WriteByte('}')
	}
	if r.Intn(6) == 0 {
		b.WriteString(`,"extra":`)
		b.WriteString(randScalar(r))
	}
	b.WriteByte('}')
	return b.String()
}

func randText(r *rand.Rand) string {
	alphabet := []string{"a", "b", " ", "中", `\"`, `\\`, `\n`, `\u00e9`, `\ud83d\ude00`, "\\t", "{", "}", "[", "]", ":", ","}
	var b strings.Builder
	for i := 0; i < r.Intn(6); i++ {
		b.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	return b.String()
}

func randScalar(r *rand.Rand) string {
	switch r.Intn(6) {
	case 0:
		return "null"
	case 1:
		return "true"
	case 2:
		return fmt.Sprint(r.Intn(100000))
	case 3:
		return `"` + randText(r) + `"`
	case 4:
		return "[]"
	default:
		return "{}"
	}
}

func TestFastCleanRandomized(t *testing.T) {
	r := rand.New(rand.NewSource(20261020))
	for i := 0; i < 200000; i++ {
		in := randChunk(r)
		want := cleanChunkJSON(in)
		got := fastResult(in)
		if !equivalentJSON(want, got) {
			t.Fatalf("随机用例 %d 不一致\n in : %s\nwant: %s\n got: %s", i, in, want, got)
		}
		// 快路径自己产出的必须是合法 JSON（当输入合法时）
		if _, ok := cleanChunkInto(nil, []byte(in), &chunkMeta{}); ok {
			var probe any
			if err := json.Unmarshal([]byte(got), &probe); err != nil {
				t.Fatalf("随机用例 %d 产出非法 JSON: %v\n in: %s\nout: %s", i, err, in, got)
			}
		}
	}
}

// FuzzFastClean：契约是"合法 JSON 输入 → 语义一致输出"；非法输入只要求不 panic、
// 不产出比原实现更差的东西（两侧都只是透传/近似透传，不承诺逐字节一致）。
func FuzzFastClean(f *testing.F) {
	for _, s := range fastCorpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if _, ok := cleanChunkInto(nil, []byte(in), &chunkMeta{}); !ok {
			return // 拒绝是允许的：调用方会回退到原实现
		}
		var probe any
		if json.Unmarshal([]byte(in), &probe) != nil {
			return // 非法 JSON：不承诺语义
		}
		want := cleanChunkJSON(in)
		got := fastResult(in)
		if !equivalentJSON(want, got) {
			t.Fatalf("fuzz 不一致\n in : %q\nwant: %s\n got: %s", in, want, got)
		}
		var out any
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatalf("快路径产出非法 JSON: %v\n in: %q\nout: %s", err, in, got)
		}
	})
}

// ---------- 2. 元数据一致性 ----------

func TestFastMetaUsageAndFinish(t *testing.T) {
	cases := []string{
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":6,"total_tokens":15}}`,
		`{"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":1}}`,
		`{"choices":[{"index":0,"delta":{}}],"usage":null}`,
		`{"choices":[{"index":0,"delta":{}}],"usage":"x"}`,
		`{"choices":[{"index":0,"delta":{}}],"usage":{}}`,
	}
	for i, in := range cases {
		var m chunkMeta
		if _, ok := cleanChunkInto(nil, []byte(in), &m); !ok {
			t.Fatalf("case %d 快路径拒绝", i)
		}
		var chunk map[string]any
		_ = json.Unmarshal([]byte(in), &chunk)
		var wantUsage map[string]any
		if u, ok := chunk["usage"].(map[string]any); ok {
			wantUsage = u
		}
		var gotUsage map[string]any
		if len(m.usage) > 0 {
			if err := json.Unmarshal(m.usage, &gotUsage); err != nil {
				t.Fatalf("case %d usage 不是合法 JSON: %v (%s)", i, err, m.usage)
			}
		}
		if !reflect.DeepEqual(wantUsage, gotUsage) {
			t.Fatalf("case %d usage 不一致 want=%v got=%v", i, wantUsage, gotUsage)
		}
		wantFinish := noteFinishReason("", chunk)
		if m.finish != wantFinish {
			t.Fatalf("case %d finish 不一致 want=%q got=%q", i, wantFinish, m.finish)
		}
	}
}

func TestFastMetaFirstToken(t *testing.T) {
	yes := []string{
		`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":" "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"r"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning":"r"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"function_call":{"name":"x"}}}]}`,
		`{"choices":[{"index":0,"delta":{"function_call":{"arguments":"{}"}}}]}`,
	}
	no := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":null}}]}`,
		`{"choices":[{"index":0,"delta":{}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":""}}]}}]}`,
		`{"choices":[]}`,
	}
	for _, in := range yes {
		var m chunkMeta
		if _, ok := cleanChunkInto(nil, []byte(in), &m); !ok {
			t.Fatalf("拒绝: %s", in)
		}
		var chunk map[string]any
		_ = json.Unmarshal([]byte(in), &chunk)
		if !hasFirstTokenDelta(chunk) {
			t.Fatalf("旧实现认为不是首 token，用例设计错误: %s", in)
		}
		if !m.firstToken {
			t.Fatalf("新实现漏判首 token: %s", in)
		}
	}
	for _, in := range no {
		var m chunkMeta
		if _, ok := cleanChunkInto(nil, []byte(in), &m); !ok {
			t.Fatalf("拒绝: %s", in)
		}
		var chunk map[string]any
		_ = json.Unmarshal([]byte(in), &chunk)
		if hasFirstTokenDelta(chunk) {
			t.Fatalf("旧实现认为是首 token，用例设计错误: %s", in)
		}
		if m.firstToken {
			t.Fatalf("新实现误判首 token: %s", in)
		}
	}
}

// ---------- 3. 端到端 ----------

type recordWriter struct {
	h       http.Header
	buf     bytes.Buffer
	flushes int
}

func (w *recordWriter) Header() http.Header         { return w.h }
func (w *recordWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *recordWriter) WriteHeader(int)             {}
func (w *recordWriter) Flush()                      { w.flushes++ }

func captureStream(t *testing.T, body string, mode fastPathModeT) string {
	t.Helper()
	old := fastPathMode.Load()
	fastPathMode.Store(int32(mode))
	defer fastPathMode.Store(old)

	acc := &Account{Path: "verify.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := &recordWriter{h: make(http.Header)}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	streamChatResponse(w, req, resp, "test-model", 1, acc, profileForEdition("cn"), time.Now())
	return w.buf.String()
}

func parseSSEPayloads(t *testing.T, stream string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		out = append(out, strings.TrimPrefix(line, "data: "))
	}
	return out
}

func TestStreamFastPathEquivalence(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	bodies := map[string]string{
		"tool-call 全流程": strings.Join([]string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"","logprobs":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"I will check."},"finish_reason":"","logprobs":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]},"finish_reason":"","logprobs":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"git status\"}"}}]},"finish_reason":"","logprobs":null}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"function_call":{"arguments":"","name":""},"role":"assistant"},"finish_reason":"tool_calls","logprobs":null}],"usage":{"prompt_tokens":9,"completion_tokens":6}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n",
		"纯文本 + 推理": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"reasoning_content":"思考"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n",
		"没有 finish_reason": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"content":"半截"}}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n",
		"含非 JSON 行": strings.Join([]string{
			`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			`data: event: ping`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n",
		"双 data 前缀": "data: data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
	}
	for name, body := range bodies {
		oldOut := captureStream(t, body, fastPathOff)
		newOut := captureStream(t, body, fastPathOn)
		shadowOut := captureStream(t, body, fastPathShadow)

		if oldOut != newOut {
			a := parseSSEPayloads(t, oldOut)
			b := parseSSEPayloads(t, newOut)
			if len(a) != len(b) {
				t.Fatalf("[%s] 事件数不同 old=%d new=%d\nold=%q\nnew=%q", name, len(a), len(b), oldOut, newOut)
			}
			for i := range a {
				if !equivalentJSON(a[i], b[i]) {
					t.Fatalf("[%s] 第 %d 个事件语义不同\nold=%s\nnew=%s", name, i, a[i], b[i])
				}
			}
		}
		// 影子模式必须与原路径逐字节一致
		if shadowOut != oldOut {
			t.Fatalf("[%s] shadow 输出与原路径不同\nold=%q\nshadow=%q", name, oldOut, shadowOut)
		}
	}
}
