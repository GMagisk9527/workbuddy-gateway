package main

// /v1/responses 出口的字节级快路径（P2）。
//
// 原实现对每个上游分片：Unmarshal 成 map[string]any → 取出 delta 字段 → 每个事件
// json.Marshal 一次 → fmt.Fprintf 写出。单事件约 9.7 µs，其中解析约 4.8 µs、
// 序列化约 1.5 µs、flush 约 2.6 µs。
//
// 这里把"解析"换成一次结构扫描（只取需要的字段，零拷贝指向输入），把三个逐 token
// 的 delta 事件换成"前缀 + 原始转义体 + 序号"的直接拼装（上游的字符串已经是 JSON
// 转义过的，可以原样复用，不需要再解码再编码）。
//
// 语义边界：
//   - 只在能完整识别结构时返回 ok；任何异常（非对象、重复键、超过上限）都回退
//     到原来的 map 路径。
//   - 事件顺序、sequence_number、item id 生成时机与原实现完全一致。
//   - 输出的 JSON 键顺序与原实现不同（原实现按字母序），但语义相同。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

const (
	respMaxChoices = 8
	respMaxTools   = 32
)

type respToolCallView struct {
	index int
	id    []byte
	typ   []byte
	name  []byte
	args  []byte
}

// respChunkView 是一个上游分片的结构化视图，所有切片都指向输入缓冲区。
type respChunkView struct {
	usage    []byte
	finish   string
	firstTok bool

	nChoices  int
	reasoning [respMaxChoices][]byte
	content   [respMaxChoices][]byte
	refusal   [respMaxChoices][]byte
	toolOff   [respMaxChoices]int
	toolCnt   [respMaxChoices]int

	nTools int
	tools  [respMaxTools]respToolCallView
}

var (
	keyChoicesB = []byte(`choices`)
	keyUsageB   = []byte(`usage`)
	keyDeltaB   = []byte(`delta`)
	keyFinishB  = []byte(`finish_reason`)
	keyContB    = []byte(`content`)
	keyRefusalB = []byte(`refusal`)
	keyReasB    = []byte(`reasoning_content`)
	keyReas2B   = []byte(`reasoning`)
	keyToolsB   = []byte(`tool_calls`)
	keyFuncB    = []byte(`function`)
	keyNameB    = []byte(`name`)
	keyArgsB    = []byte(`arguments`)
	keyIDB      = []byte(`id`)
	keyTypeB    = []byte(`type`)
	keyIndexB   = []byte(`index`)
)

// keySet 检测同一对象内的重复键：重复键在原实现里经 map 合并，快路径无法等价表达。
type keySet struct {
	n int
	k [10][]byte
}

func (s *keySet) seen(key []byte) bool {
	for i := 0; i < s.n; i++ {
		if bytes.Equal(s.k[i], key) {
			return true
		}
	}
	if s.n >= len(s.k) {
		return true
	}
	s.k[s.n] = key
	s.n++
	return false
}

func parseResponsesChunk(src []byte, v *respChunkView) bool {
	v.usage = v.usage[:0]
	v.finish = ""
	v.firstTok = false
	v.nChoices = 0
	v.nTools = 0

	i := skipWS(src, 0)
	if i >= len(src) || src[i] != '{' {
		return false
	}
	i++
	var ks keySet
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return false
		}
		if src[i] == '}' {
			i++
			break
		}
		start := i
		n, ok := skipString(src, i)
		if !ok {
			return false
		}
		key := src[start+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || ks.seen(key) {
			return false
		}
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return false
		}
		i = skipWS(src, i+1)
		switch {
		case bytes.Equal(key, keyUsageB) && i < len(src) && src[i] == '{':
			end, okv := skipValue(src, i)
			if !okv {
				return false
			}
			v.usage = append(v.usage[:0], src[i:end]...)
			i = end
		case bytes.Equal(key, keyChoicesB) && i < len(src) && src[i] == '[':
			var okc bool
			i, okc = respParseChoices(src, i, v)
			if !okc {
				return false
			}
		default:
			end, okv := skipValue(src, i)
			if !okv {
				return false
			}
			i = end
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			i++
			break
		}
		return false
	}
	return skipWS(src, i) == len(src)
}

func respParseChoices(src []byte, i int, v *respChunkView) (int, bool) {
	i++ // '['
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == ']' {
			return i + 1, true
		}
		if src[i] != '{' {
			end, ok := skipValue(src, i)
			if !ok {
				return i, false
			}
			i = end
		} else {
			if v.nChoices >= respMaxChoices {
				return i, false
			}
			var ok bool
			i, ok = respParseChoice(src, i, v, v.nChoices)
			if !ok {
				return i, false
			}
			v.nChoices++
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == ']' {
			return i + 1, true
		}
		return i, false
	}
}

func respParseChoice(src []byte, i int, v *respChunkView, c int) (int, bool) {
	v.reasoning[c] = nil
	v.content[c] = nil
	v.refusal[c] = nil
	v.toolCnt[c] = 0
	i++ // '{'
	var ks keySet
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == '}' {
			return i + 1, true
		}
		start := i
		n, ok := skipString(src, i)
		if !ok {
			return i, false
		}
		key := src[start+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || ks.seen(key) {
			// 转义键（如 "\u0063ontent"）解码后与字面量相同，按字节比较会漏字段；
			// 重复键在原实现里经 map 合并，快路径无法等价表达 —— 两者都回退。
			return i, false
		}
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return i, false
		}
		i = skipWS(src, i+1)
		switch {
		case bytes.Equal(key, keyFinishB) && i < len(src) && src[i] == '"':
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			if v.finish == "" {
				if s := jsonStringValue(src[i:end]); s != "" {
					v.finish = s
				}
			}
			i = end
		case bytes.Equal(key, keyDeltaB) && i < len(src) && src[i] == '{':
			var okd bool
			i, okd = respParseDelta(src, i, v, c)
			if !okd {
				return i, false
			}
		default:
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			i = end
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			return i + 1, true
		}
		return i, false
	}
}

func respParseDelta(src []byte, i int, v *respChunkView, c int) (int, bool) {
	startTools := v.nTools
	v.toolOff[c] = startTools
	i++ // '{'
	var ks keySet
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == '}' {
			v.toolCnt[c] = v.nTools - startTools
			return i + 1, true
		}
		start := i
		n, ok := skipString(src, i)
		if !ok {
			return i, false
		}
		key := src[start+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || ks.seen(key) {
			// 转义键（如 "\u0063ontent"）解码后与字面量相同，按字节比较会漏字段；
			// 重复键在原实现里经 map 合并，快路径无法等价表达 —— 两者都回退。
			return i, false
		}
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return i, false
		}
		i = skipWS(src, i+1)
		switch {
		case bytes.Equal(key, keyContB), bytes.Equal(key, keyRefusalB), bytes.Equal(key, keyReasB), bytes.Equal(key, keyReas2B):
			if i < len(src) && src[i] == '"' {
				end, okv := skipValue(src, i)
				if !okv {
					return i, false
				}
				raw := src[i+1 : end-1]
				if len(raw) > 0 {
					// 与原路径对齐：只有 reasoning_content 会产出 reasoning 增量；
					// reasoning 仅参与首 token 判定（hasFirstTokenDelta 认它）。
					switch {
					case bytes.Equal(key, keyContB):
						v.content[c] = raw
					case bytes.Equal(key, keyRefusalB):
						v.refusal[c] = raw
					case bytes.Equal(key, keyReasB):
						v.reasoning[c] = raw
					}
					v.firstTok = true
				}
				i = end
				break
			}
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			i = end
		case bytes.Equal(key, keyToolsB) && i < len(src) && src[i] == '[':
			beforeTools := v.nTools
			end, okv := respParseToolCalls(src, i, v)
			if !okv {
				return i, false
			}
			for t := beforeTools; t < v.nTools; t++ {
				if v.tools[t].name != nil || len(v.tools[t].args) > 0 {
					v.firstTok = true
				}
			}
			i = end
		case bytes.Equal(key, keyFuncB) && i < len(src) && src[i] == '{':
			// 旧版 function_call：hasFirstTokenDelta 也会把它当成工具调用
			var tv respToolCallView
			end, okv := respParseFunction(src, i, &tv)
			if !okv {
				return i, false
			}
			if tv.name != nil || len(tv.args) > 0 {
				v.firstTok = true
			}
			i = end
		default:
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			i = end
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			v.toolCnt[c] = v.nTools - startTools
			return i + 1, true
		}
		return i, false
	}
}

func respParseToolCalls(src []byte, i int, v *respChunkView) (int, bool) {
	i++ // '['
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == ']' {
			return i + 1, true
		}
		if src[i] != '{' {
			end, ok := skipValue(src, i)
			if !ok {
				return i, false
			}
			i = end
		} else {
			if v.nTools >= respMaxTools {
				return i, false
			}
			t := &v.tools[v.nTools]
			*t = respToolCallView{}
			var ok bool
			i, ok = respParseToolCall(src, i, t)
			if !ok {
				return i, false
			}
			v.nTools++
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == ']' {
			return i + 1, true
		}
		return i, false
	}
}

func respParseToolCall(src []byte, i int, t *respToolCallView) (int, bool) {
	i++ // '{'
	var ks keySet
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == '}' {
			return i + 1, true
		}
		start := i
		n, ok := skipString(src, i)
		if !ok {
			return i, false
		}
		key := src[start+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || ks.seen(key) {
			// 转义键（如 "\u0063ontent"）解码后与字面量相同，按字节比较会漏字段；
			// 重复键在原实现里经 map 合并，快路径无法等价表达 —— 两者都回退。
			return i, false
		}
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return i, false
		}
		i = skipWS(src, i+1)
		switch {
		case bytes.Equal(key, keyFuncB) && i < len(src) && src[i] == '{':
			end, okv := respParseFunction(src, i, t)
			if !okv {
				return i, false
			}
			i = end
		case bytes.Equal(key, keyIndexB):
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			if f, err := strconv.ParseFloat(string(src[i:end]), 64); err == nil {
				t.index = int(f)
			}
			i = end
		default:
			end, okv := skipValue(src, i)
			if !okv {
				return i, false
			}
			val := src[i:end]
			switch {
			case bytes.Equal(key, keyIDB) && len(val) >= 2 && val[0] == '"':
				t.id = val[1 : len(val)-1]
			case bytes.Equal(key, keyTypeB) && len(val) >= 2 && val[0] == '"':
				t.typ = val[1 : len(val)-1]
			}
			i = end
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			return i + 1, true
		}
		return i, false
	}
}

func respParseFunction(src []byte, i int, t *respToolCallView) (int, bool) {
	i++ // '{'
	var ks keySet
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return i, false
		}
		if src[i] == '}' {
			return i + 1, true
		}
		start := i
		n, ok := skipString(src, i)
		if !ok {
			return i, false
		}
		key := src[start+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || ks.seen(key) {
			// 转义键（如 "\u0063ontent"）解码后与字面量相同，按字节比较会漏字段；
			// 重复键在原实现里经 map 合并，快路径无法等价表达 —— 两者都回退。
			return i, false
		}
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return i, false
		}
		i = skipWS(src, i+1)
		end, okv := skipValue(src, i)
		if !okv {
			return i, false
		}
		val := src[i:end]
		if len(val) >= 2 && val[0] == '"' {
			switch {
			case bytes.Equal(key, keyNameB):
				t.name = val[1 : len(val)-1]
			case bytes.Equal(key, keyArgsB):
				t.args = val[1 : len(val)-1]
			}
		}
		i = end
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			return i + 1, true
		}
		return i, false
	}
}

// decodeJSONBody 把 JSON 字符串体（不含两侧引号）解成 Go 字符串。
func decodeJSONBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	buf := make([]byte, 0, len(raw)+2)
	buf = append(buf, '"')
	buf = append(buf, raw...)
	buf = append(buf, '"')
	var s string
	if json.Unmarshal(buf, &s) != nil {
		return string(raw)
	}
	return s
}

// ---- 逐 token 事件的直接拼装 ----

func respContentDeltaPrefix(dst []byte, event, itemID string, outputIndex, contentIndex int) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, event...)
	dst = append(dst, "\ndata: {\"type\":\""...)
	dst = append(dst, event...)
	dst = append(dst, "\",\"item_id\":\""...)
	dst = append(dst, itemID...)
	dst = append(dst, "\",\"output_index\":"...)
	dst = strconv.AppendInt(dst, int64(outputIndex), 10)
	dst = append(dst, ",\"content_index\":"...)
	dst = strconv.AppendInt(dst, int64(contentIndex), 10)
	dst = append(dst, ",\"delta\":\""...)
	return dst
}

func respReasoningDeltaPrefix(dst []byte, itemID string, outputIndex int) []byte {
	dst = append(dst, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\""...)
	dst = append(dst, itemID...)
	dst = append(dst, "\",\"output_index\":"...)
	dst = strconv.AppendInt(dst, int64(outputIndex), 10)
	dst = append(dst, ",\"summary_index\":0,\"delta\":\""...)
	return dst
}

func respToolDeltaPrefix(dst []byte, itemID string, outputIndex int) []byte {
	dst = append(dst, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\""...)
	dst = append(dst, itemID...)
	dst = append(dst, "\",\"output_index\":"...)
	dst = strconv.AppendInt(dst, int64(outputIndex), 10)
	dst = append(dst, ",\"delta\":\""...)
	return dst
}

// emitRawDelta 写出一个 delta 事件：prefix 已含 event 行与 data 前缀，
// raw 是上游原样转义体，seq 追加在末尾（键顺序与 map 序列化不同，语义一致）。
func emitRawDelta(w io.Writer, fl http.Flusher, buf *[]byte, prefix, raw []byte, seq *int) error {
	b := append((*buf)[:0], prefix...)
	b = append(b, raw...)
	b = append(b, "\",\"sequence_number\":"...)
	b = strconv.AppendInt(b, int64(*seq), 10)
	b = append(b, '}', '\n', '\n')
	*buf = b
	*seq = *seq + 1
	return writeSSE(w, fl, b)
}

// writeSSE 写一帧并 flush；写失败返回错误，由调用方停止读取上游。
func writeSSE(w io.Writer, fl http.Flusher, buf []byte) error {
	if _, err := w.Write(buf); err != nil {
		return err
	}
	fl.Flush()
	return nil
}

// applyToolCallView 与 applyToolCallDelta 等价，入参是零拷贝视图。
func applyToolCallView(toolCalls map[int]*mergedToolCall, order *[]int, tv *respToolCallView) {
	idx := tv.index
	st, exists := toolCalls[idx]
	if !exists {
		st = &mergedToolCall{Type: "function"}
		toolCalls[idx] = st
		*order = append(*order, idx)
	}
	if id := decodeJSONBody(tv.id); id != "" {
		st.ID = id
	}
	if typ := decodeJSONBody(tv.typ); typ != "" {
		st.Type = typ
	}
	if name := decodeJSONBody(tv.name); name != "" {
		st.Name = name
	}
	if args := decodeJSONBody(tv.args); args != "" {
		st.Args.WriteString(args)
	}
}

// jsonEscapeBody 把一个 Go 字符串转成 JSON 字符串体（不含两侧引号）。
// 仅用于回退路径：慢路径拿到的是已解码文本，需要写回统一的 raw 累计器。
func jsonEscapeBody(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return []byte(s)
	}
	return b[1 : len(b)-1]
}
