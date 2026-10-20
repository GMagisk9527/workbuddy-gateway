package main

// /v1/responses 快路径的验证：同一份上游流，分别走原路径与快路径，
// 逐事件比对（归一化随机 id 与时间戳后做语义比较）。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	reRespID = regexp.MustCompile(`resp_[0-9a-zA-Z]+`)
	reMsgID  = regexp.MustCompile(`msg_[0-9a-zA-Z]+`)
	reRsID   = regexp.MustCompile(`rs_[0-9a-zA-Z]+`)
	reFcID   = regexp.MustCompile(`fc_[0-9a-zA-Z]+`)
	reCallID = regexp.MustCompile(`call_[0-9a-zA-Z]+`)
	reStamp  = regexp.MustCompile(`"(created|created_at|completed_at)":\s*\d+`)
)

func normalizeRespPayload(s string) string {
	s = reRespID.ReplaceAllString(s, "resp_X")
	s = reMsgID.ReplaceAllString(s, "msg_X")
	s = reRsID.ReplaceAllString(s, "rs_X")
	s = reFcID.ReplaceAllString(s, "fc_X")
	s = reCallID.ReplaceAllString(s, "call_X")
	s = reStamp.ReplaceAllString(s, `"$1":0`)
	return s
}

type sseEvent struct {
	name string
	data string
}

func splitSSE(t *testing.T, stream string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(stream, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		out = append(out, ev)
	}
	return out
}

func captureResponsesStream(t *testing.T, body string, mode fastPathModeT) string {
	t.Helper()
	old := fastPathMode.Load()
	fastPathMode.Store(int32(mode))
	defer fastPathMode.Store(old)

	acc := &Account{Path: "verify.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := &recordWriter{h: make(http.Header)}
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	streamResponsesResponse(w, req, resp, "test-model", 1, acc, profileForEdition("cn"), time.Now())
	return w.buf.String()
}

func compareResponsesStreams(t *testing.T, name, oldOut, newOut string) {
	t.Helper()
	a := splitSSE(t, oldOut)
	b := splitSSE(t, newOut)
	if len(a) != len(b) {
		t.Fatalf("[%s] 事件数不同 old=%d new=%d\n--- old ---\n%s\n--- new ---\n%s", name, len(a), len(b), oldOut, newOut)
	}
	for i := range a {
		if a[i].name != b[i].name {
			t.Fatalf("[%s] 第 %d 个事件名不同 old=%q new=%q", name, i, a[i].name, b[i].name)
		}
		if a[i].data == "[DONE]" || b[i].data == "[DONE]" {
			if a[i].data != b[i].data {
				t.Fatalf("[%s] 第 %d 个事件 [DONE] 不一致", name, i)
			}
			continue
		}
		var x, y any
		if err := json.Unmarshal([]byte(normalizeRespPayload(a[i].data)), &x); err != nil {
			t.Fatalf("[%s] 旧路径第 %d 个事件不是合法 JSON: %v\n%s", name, i, err, a[i].data)
		}
		if err := json.Unmarshal([]byte(normalizeRespPayload(b[i].data)), &y); err != nil {
			t.Fatalf("[%s] 新路径第 %d 个事件不是合法 JSON: %v\n%s", name, i, err, b[i].data)
		}
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("[%s] 第 %d 个事件语义不同\n--- old ---\n%s\n--- new ---\n%s", name, i, a[i].data, b[i].data)
		}
	}
}

func respChunk(delta string, finish string) string {
	fin := "null"
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fin + `}]}` + "\n\n"
}

func TestResponsesFastPathEquivalence(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	bodies := map[string]string{
		"纯文本": respChunk(`{"role":"assistant"}`, "") +
			respChunk(`{"content":"Hello"}`, "") +
			respChunk(`{"content":" world"}`, "") +
			respChunk(`{"content":" 中文\u00e9\ud83d\ude00"}`, "") +
			respChunk(`{}`, "stop") +
			"data: [DONE]\n\n",

		"先推理后正文": respChunk(`{"reasoning_content":"think 1"}`, "") +
			respChunk(`{"reasoning_content":"think 2"}`, "") +
			respChunk(`{"content":"answer"}`, "") +
			respChunk(`{}`, "stop") +
			"data: [DONE]\n\n",

		"同片既有推理又有正文": respChunk(`{"reasoning_content":"r","content":"c"}`, "") +
			respChunk(`{}`, "stop") +
			"data: [DONE]\n\n",

		"工具调用": respChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":""}}]}`, "") +
			respChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}`, "") +
			respChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}`, "") +
			respChunk(`{}`, "tool_calls") +
			"data: [DONE]\n\n",

		"两个工具并行": respChunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"A","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"B","arguments":"{}"}}]}`, "") +
			respChunk(`{}`, "tool_calls") +
			"data: [DONE]\n\n",

		"带 usage": respChunk(`{"content":"x"}`, "") +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":4}}}` + "\n\n" +
			"data: [DONE]\n\n",

		"没有 finish_reason": respChunk(`{"content":"半截"}`, "") + "data: [DONE]\n\n",

		"非 JSON 行": respChunk(`{"content":"x"}`, "") + "data: event: ping\n\n" + respChunk(`{}`, "stop") + "data: [DONE]\n\n",

		"空 delta": respChunk(`{}`, "") + respChunk(`{"content":""}`, "") + respChunk(`{"content":null}`, "") + respChunk(`{}`, "stop") + "data: [DONE]\n\n",

		"转义字符": respChunk(`{"content":"a\"b\\\\c\nd\t<&>"}`, "") + respChunk(`{}`, "stop") + "data: [DONE]\n\n",
	}
	for name, body := range bodies {
		oldOut := captureResponsesStream(t, body, fastPathOff)
		newOut := captureResponsesStream(t, body, fastPathOn)
		compareResponsesStreams(t, name, oldOut, newOut)
	}
}

// randResponsesStream 生成随机的上游流（内容/推理/工具/空 delta 混合）。
func randResponsesStream(r *rand.Rand) string {
	var sb strings.Builder
	n := 1 + r.Intn(12)
	for i := 0; i < n; i++ {
		switch r.Intn(6) {
		case 0:
			sb.WriteString(respChunk(`{"content":"`+randText(r)+`"}`, ""))
		case 1:
			sb.WriteString(respChunk(`{"reasoning_content":"`+randText(r)+`"}`, ""))
		case 2:
			sb.WriteString(respChunk(`{"content":"`+randText(r)+`","reasoning_content":"`+randText(r)+`"}`, ""))
		case 3:
			sb.WriteString(respChunk(`{}`, ""))
		case 4:
			sb.WriteString(respChunk(`{"tool_calls":[{"index":`+fmt.Sprint(r.Intn(2))+`,"function":{"name":"T`+fmt.Sprint(r.Intn(3))+`","arguments":"`+randText(r)+`"}}]}`, ""))
		case 5:
			sb.WriteString(respChunk(`{"content":null,"tool_calls":[]}`, ""))
		}
	}
	sb.WriteString(respChunk(`{}`, "stop"))
	if r.Intn(2) == 0 {
		sb.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + fmt.Sprint(r.Intn(100)) + `,"completion_tokens":` + fmt.Sprint(r.Intn(50)) + `}}` + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

func TestResponsesFastPathRandomized(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())
	r := rand.New(rand.NewSource(20261020))
	for i := 0; i < 2000; i++ {
		body := randResponsesStream(r)
		oldOut := captureResponsesStream(t, body, fastPathOff)
		newOut := captureResponsesStream(t, body, fastPathOn)
		compareResponsesStreams(t, fmt.Sprintf("random-%d", i), oldOut, newOut)
	}
}

// TestResponsesParserAccepts 确保上面的等价测试真的走了快路径，而不是全部静默回退。
func TestResponsesParserAccepts(t *testing.T) {
	bodies := []string{
		respChunk(`{"role":"assistant"}`, ""),
		respChunk(`{"content":"Hello world"}`, ""),
		respChunk(`{"content":" 中文\u00e9\ud83d\ude00"}`, ""),
		respChunk(`{"reasoning_content":"think"}`, ""),
		respChunk(`{"reasoning":"r"}`, ""),
		respChunk(`{"content":"c","reasoning_content":"r"}`, ""),
		respChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}`, ""),
		respChunk(`{}`, "stop"),
		respChunk(`{"content":null}`, ""),
		respChunk(`{"content":""}`, ""),
		respChunk(`{}`, ""),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":1}}` + "\n\n",
	}
	var v respChunkView
	for i, body := range bodies {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			if !parseResponsesChunk([]byte(payload), &v) {
				t.Fatalf("case %d 解析器拒绝：%s", i, payload)
			}
		}
	}
}
