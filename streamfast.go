package main

// 热路径结构化改写（P1）。
//
// 背景：streamChatResponse 对每个上游分片做两遍 JSON 解析 —— 一遍取 usage/
// finish_reason，另一遍在 cleanChunkJSON 里改两个字段后再整体 Marshal。pprof
// 显示后者占单事件 CPU 的 60%、JSON 合计 93%。
//
// 本文件提供 cleanChunkInto：一次结构扫描完成 cleanChunkJSON 的全部语义，
// 顺带把流式循环需要的元数据（usage 片段 / finish_reason / 首 token）提出来，
// 不再建 map、不再重新序列化。
//
// 安全边界：只处理"形状可识别"的输入；任何异常（非 JSON、非对象、类型不符、
// 括号不平衡）都返回 ok=false，由调用方回退到 cleanChunkJSON，保证语义不变。
//
// 开关：环境变量 WBG_FASTPATH
//   on（默认）  走快路径
//   off         完全走原路径（用于回滚）
//   shadow      走原路径 + 影子比对（诊断用，会变慢并打日志）

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

type fastPathModeT int32

const (
	fastPathOff fastPathModeT = iota
	fastPathShadow
	fastPathOn
)

var fastPathMode atomic.Int32

func init() {
	mode := fastPathOn
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WBG_FASTPATH"))) {
	case "off", "0", "false", "no":
		mode = fastPathOff
	case "shadow":
		mode = fastPathShadow
	}
	fastPathMode.Store(int32(mode))
}

var (
	keyChoices    = []byte(`choices`)
	keyUsage      = []byte(`usage`)
	keyDelta      = []byte(`delta`)
	keyFinishRsn  = []byte(`finish_reason`)
	keyFuncCall   = []byte(`function_call`)
	keyName       = []byte(`name`)
	keyArguments  = []byte(`arguments`)
	keyContent    = []byte(`content`)
	keyReasoning  = []byte(`reasoning_content`)
	keyReasoning2 = []byte(`reasoning`)
	keyToolCalls  = []byte(`tool_calls`)
	keyFunction   = []byte(`function`)
	doneMarker    = []byte("[DONE]")
	dataPrefix    = []byte("data:")
)

// chunkMeta 是流式循环需要的、从分片里顺手提取出来的元数据。
type chunkMeta struct {
	usage      []byte // 最近一个 usage 对象的原始字节（整条流通常只有末尾一个）
	finish     string // 本分片内第一个非空 finish_reason
	firstToken bool   // 本分片是否包含有效内容/推理/工具增量
}

func (m *chunkMeta) reset() {
	m.usage = m.usage[:0]
	m.finish = ""
	m.firstToken = false
}

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString 跳过从 i 开始的一个 JSON 字符串，返回其后的下标。
func skipString(b []byte, i int) (int, bool) {
	if i >= len(b) || b[i] != '"' {
		return i, false
	}
	i++
	for i < len(b) {
		switch b[i] {
		case '\\':
			if i+1 >= len(b) {
				return i, false
			}
			i += 2
		case '"':
			return i + 1, true
		default:
			if b[i] < 0x20 {
				return i, false // 裸控制字符：encoding/json 也判非法
			}
			i++
		}
	}
	return i, false
}

// skipValue 跳过一个任意 JSON 值，返回其后的下标。
func skipValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return i, false
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				n, ok := skipString(b, i)
				if !ok {
					return i, false
				}
				i = n
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	}
	start := i
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			if i == start {
				return i, false
			}
			return i, true
		}
		i++
	}
	if i > start {
		return i, true
	}
	return i, false
}

// spanIsEmpty 对应 isEmptyValue：nil / "" / [] / {} 视为空。
func spanIsEmpty(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	switch b[0] {
	case '"':
		return len(b) == 2
	case '[':
		if len(b) < 2 {
			return false
		}
		for _, c := range b[1 : len(b)-1] {
			switch c {
			case ' ', '\t', '\n', '\r':
			default:
				return false
			}
		}
		return true
	case '{':
		if len(b) < 2 {
			return false
		}
		for _, c := range b[1 : len(b)-1] {
			switch c {
			case ' ', '\t', '\n', '\r':
			default:
				return false
			}
		}
		return true
	case 'n':
		return len(b) == 4 && b[1] == 'u' && b[2] == 'l' && b[3] == 'l'
	}
	return false
}

// jsonStringValue 解出一个 JSON 字符串字面量；无转义时零分配。
func jsonStringValue(b []byte) string {
	if len(b) < 2 || b[0] != '"' {
		return ""
	}
	raw := b[1 : len(b)-1]
	for _, c := range raw {
		if c == '\\' {
			var s string
			if json.Unmarshal(b, &s) != nil {
				return ""
			}
			return s
		}
	}
	return string(raw)
}

// functionCallIsStub 对应 isEmptyFunctionCall：非对象返回 false。
func functionCallIsStub(b []byte) bool {
	if len(b) < 2 || b[0] != '{' {
		return false
	}
	name, args := "", ""
	i := 1
	for {
		i = skipWS(b, i)
		if i >= len(b) || b[i] == '}' {
			break
		}
		ks := i
		n, ok := skipString(b, i)
		if !ok {
			return false
		}
		key := b[ks+1 : n-1]
		i = skipWS(b, n)
		if i >= len(b) || b[i] != ':' {
			return false
		}
		i = skipWS(b, i+1)
		vs := i
		i, ok = skipValue(b, i)
		if !ok {
			return false
		}
		val := b[vs:i]
		if bytes.IndexByte(key, '\\') >= 0 {
			return false // 转义键：保守起见不判定为空壳
		}
		if len(val) > 0 && val[0] == '"' {
			switch {
			case bytes.Equal(key, keyName):
				name = jsonStringValue(val)
			case bytes.Equal(key, keyArguments):
				args = jsonStringValue(val)
			}
		}
		i = skipWS(b, i)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		break
	}
	return strings.TrimSpace(name) == "" && strings.TrimSpace(args) == ""
}

// objectHasNamedFunction 判断 tool_calls 的一个元素是否让首 token 成立：
// 与 hasFirstTokenDelta 一致 —— function.name 以字符串出现即算，或 arguments 非空。
func objectHasNamedFunction(b []byte) bool {
	if len(b) < 2 || b[0] != '{' {
		return false
	}
	i := 1
	for {
		i = skipWS(b, i)
		if i >= len(b) || b[i] == '}' {
			return false
		}
		ks := i
		n, ok := skipString(b, i)
		if !ok {
			return false
		}
		key := b[ks+1 : n-1]
		i = skipWS(b, n)
		if i >= len(b) || b[i] != ':' {
			return false
		}
		i = skipWS(b, i+1)
		vs := i
		i, ok = skipValue(b, i)
		if !ok {
			return false
		}
		if bytes.Equal(key, keyFunction) {
			return functionHasNameOrArgs(b[vs:i])
		}
		i = skipWS(b, i)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		return false
	}
}

func functionHasNameOrArgs(b []byte) bool {
	if len(b) < 2 || b[0] != '{' {
		return false
	}
	i := 1
	for {
		i = skipWS(b, i)
		if i >= len(b) || b[i] == '}' {
			return false
		}
		ks := i
		n, ok := skipString(b, i)
		if !ok {
			return false
		}
		key := b[ks+1 : n-1]
		i = skipWS(b, n)
		if i >= len(b) || b[i] != ':' {
			return false
		}
		i = skipWS(b, i+1)
		vs := i
		i, ok = skipValue(b, i)
		if !ok {
			return false
		}
		val := b[vs:i]
		if bytes.Equal(key, keyName) && len(val) >= 2 && val[0] == '"' {
			return true // name 以字符串出现即算首 token（与 hasFirstTokenDelta 一致）
		}
		if bytes.Equal(key, keyArguments) && len(val) > 2 && val[0] == '"' {
			return true // arguments 非空
		}
		i = skipWS(b, i)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		return false
	}
}

// toolCallsHaveFirstToken 遍历 tool_calls 数组，判断是否有元素让首 token 成立。
func toolCallsHaveFirstToken(b []byte) bool {
	if len(b) < 2 || b[0] != '[' {
		return false
	}
	i := 1
	for {
		i = skipWS(b, i)
		if i >= len(b) {
			return false
		}
		if b[i] == ']' {
			return false
		}
		vs := i
		n, ok := skipValue(b, i)
		if !ok {
			return false
		}
		if objectHasNamedFunction(b[vs:n]) {
			return true
		}
		i = skipWS(b, n)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		return false
	}
}

// cleanChunkInto 把 src（一个 chat.completion.chunk）清洗后追加到 dst，并填充 meta。
// ok=false 表示形状不可识别，调用方必须回退到 cleanChunkJSON。
func cleanChunkInto(dst, src []byte, m *chunkMeta) ([]byte, bool) {
	m.reset()
	i := skipWS(src, 0)
	if i >= len(src) || src[i] != '{' {
		return dst, false
	}
	base := len(dst)
	dst = append(dst, '{')
	i++

	var seenBuf [16][]byte
	seen := seenBuf[:0]
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return dst[:base], false
		}
		if src[i] == '}' {
			dst = append(dst, '}')
			i++
			break
		}
		ks := i
		n, ok := skipString(src, i)
		if !ok {
			return dst[:base], false
		}
		key := src[ks+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || keyDup(seen, key) {
			return dst[:base], false
		}
		if len(seen) >= len(seenBuf) {
			return dst[:base], false
		}
		seen = append(seen, key)
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return dst[:base], false
		}
		i = skipWS(src, i+1)
		vs := i
		// 先原样写出 "key":
		dst = append(dst, src[ks:vs]...)

		switch {
		case bytes.Equal(key, keyChoices) && i < len(src) && src[i] == '[':
			dst, i, ok = rewriteChoicesInto(src, i, dst, m)
			if !ok {
				return dst[:base], false
			}
		case bytes.Equal(key, keyUsage) && i < len(src) && src[i] == '{':
			end, okv := skipValue(src, i)
			if !okv {
				return dst[:base], false
			}
			m.usage = append(m.usage[:0], src[i:end]...)
			dst = append(dst, src[i:end]...)
			i = end
		default:
			end, okv := skipValue(src, i)
			if !okv {
				return dst[:base], false
			}
			dst = append(dst, src[vs:end]...)
			i = end
		}

		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			dst = append(dst, ',')
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			dst = append(dst, '}')
			i++
			break
		}
		return dst[:base], false
	}
	if skipWS(src, i) != len(src) {
		return dst[:base], false
	}
	return dst, true
}

func rewriteChoicesInto(src []byte, i int, dst []byte, m *chunkMeta) ([]byte, int, bool) {
	dst = append(dst, '[')
	i++
	first := true
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return dst, i, false
		}
		if src[i] == ']' {
			dst = append(dst, ']')
			return dst, i + 1, true
		}
		if !first {
			dst = append(dst, ',')
		}
		first = false
		if src[i] != '{' {
			vs := i
			n, ok := skipValue(src, i)
			if !ok {
				return dst, i, false
			}
			dst = append(dst, src[vs:n]...)
			i = n
		} else {
			var ok bool
			dst, i, ok = rewriteChoiceInto(src, i, dst, m)
			if !ok {
				return dst, i, false
			}
		}
		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == ']' {
			dst = append(dst, ']')
			return dst, i + 1, true
		}
		return dst, i, false
	}
}

func rewriteChoiceInto(src []byte, i int, dst []byte, m *chunkMeta) ([]byte, int, bool) {
	dst = append(dst, '{')
	i++
	first := true
	var seenBuf [16][]byte
	seen := seenBuf[:0]
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return dst, i, false
		}
		if src[i] == '}' {
			dst = append(dst, '}')
			return dst, i + 1, true
		}
		ks := i
		n, ok := skipString(src, i)
		if !ok {
			return dst, i, false
		}
		key := src[ks+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || keyDup(seen, key) {
			return dst, i, false
		}
		if len(seen) >= len(seenBuf) {
			return dst, i, false
		}
		seen = append(seen, key)
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return dst, i, false
		}
		i = skipWS(src, i+1)
		vs := i

		switch {
		case bytes.Equal(key, keyFinishRsn) && i < len(src) && src[i] == '"':
			end, okv := skipValue(src, i)
			if !okv {
				return dst, i, false
			}
			if m.finish == "" {
				if v := jsonStringValue(src[i:end]); v != "" {
					m.finish = v
				}
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, src[ks:vs]...) // "finish_reason":
			if end == i+2 {
				// 空 finish_reason 归一化为 null（原实现的核心语义）
				dst = append(dst, "null"...)
			} else {
				dst = append(dst, src[i:end]...)
			}
			i = end

		case bytes.Equal(key, keyDelta) && i < len(src) && src[i] == '{':
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, src[ks:vs]...) // "delta":
			var okd bool
			dst, i, okd = rewriteDeltaInto(src, i, dst, m)
			if !okd {
				return dst, i, false
			}

		default:
			end, okv := skipValue(src, i)
			if !okv {
				return dst, i, false
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, src[ks:end]...)
			i = end
		}

		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			dst = append(dst, '}')
			return dst, i + 1, true
		}
		return dst, i, false
	}
}

func rewriteDeltaInto(src []byte, i int, dst []byte, m *chunkMeta) ([]byte, int, bool) {
	dst = append(dst, '{')
	i++
	first := true
	var seenBuf [16][]byte
	seen := seenBuf[:0]
	for {
		i = skipWS(src, i)
		if i >= len(src) {
			return dst, i, false
		}
		if src[i] == '}' {
			dst = append(dst, '}')
			return dst, i + 1, true
		}
		ks := i
		n, ok := skipString(src, i)
		if !ok {
			return dst, i, false
		}
		key := src[ks+1 : n-1]
		if bytes.IndexByte(key, '\\') >= 0 || keyDup(seen, key) {
			return dst, i, false
		}
		if len(seen) >= len(seenBuf) {
			return dst, i, false
		}
		seen = append(seen, key)
		i = skipWS(src, n)
		if i >= len(src) || src[i] != ':' {
			return dst, i, false
		}
		i = skipWS(src, i+1)
		vs := i
		end, okv := skipValue(src, i)
		if !okv {
			return dst, i, false
		}
		val := src[vs:end]

		// 首 token 判定（对应 hasFirstTokenDelta）
		if len(val) > 2 && val[0] == '"' {
			switch {
			case bytes.Equal(key, keyContent),
				bytes.Equal(key, keyReasoning),
				bytes.Equal(key, keyReasoning2):
				m.firstToken = true
			}
		}
		switch {
		case bytes.Equal(key, keyToolCalls) && len(val) > 0 && val[0] == '[':
			if toolCallsHaveFirstToken(val) {
				m.firstToken = true
			}
		case bytes.Equal(key, keyFuncCall) && len(val) > 0 && val[0] == '{':
			if functionHasNameOrArgs(val) {
				m.firstToken = true
			}
		}

		drop := spanIsEmpty(val)
		if !drop && bytes.Equal(key, keyFuncCall) && functionCallIsStub(val) {
			drop = true
		}
		if !drop {
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, src[ks:vs]...)
			dst = append(dst, val...)
		}
		i = end

		i = skipWS(src, i)
		if i < len(src) && src[i] == ',' {
			i++
			continue
		}
		if i < len(src) && src[i] == '}' {
			dst = append(dst, '}')
			return dst, i + 1, true
		}
		return dst, i, false
	}
}

// stripDataPrefixBytes 是 stripDataPrefix 的字节版。
func stripDataPrefixBytes(s []byte) []byte {
	s = bytes.TrimSpace(s)
	for bytes.HasPrefix(s, dataPrefix) {
		s = bytes.TrimSpace(s[len(dataPrefix):])
	}
	return s
}

// observeFirstTokenFlag 在快路径下记录首 token 时间，语义与 observeFirstToken 一致。
func observeFirstTokenFlag(reader any) {
	if t, ok := reader.(*ttftReader); ok {
		t.once.Do(func() { t.d = time.Since(t.start) })
	}
}

// shadowCompare 只用于 WBG_FASTPATH=shadow：语义比对两条路径的输出。
func shadowCompare(oldStr string, fast []byte) bool {
	var a, b any
	if json.Unmarshal([]byte(oldStr), &a) != nil {
		return false
	}
	if json.Unmarshal(fast, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

var shadowMismatch atomic.Int64
var shadowChecked atomic.Int64

func shadowReport() {
	if n := shadowMismatch.Load(); n > 0 {
		log.Printf("[快路径影子] 不一致=%d 已比对=%d 结果=输出仍走原实现，请把这条日志反馈", n, shadowChecked.Load())
	}
}

// shadowClean 只用于 shadow 模式：跑一遍快路径并返回清洗后的字节。
func shadowClean(data []byte) []byte {
	m := chunkMeta{usage: make([]byte, 0, 64)}
	buf := make([]byte, 0, len(data)+16)
	buf, ok := cleanChunkInto(buf, data, &m)
	if !ok {
		return nil
	}
	return buf
}

// keyDup 用于检测同一对象内的重复键：重复键在原实现里经 map 合并（后者胜出），
// 快路径无法等价表达，直接回退到 cleanChunkJSON。
func keyDup(seen [][]byte, key []byte) bool {
	for _, k := range seen {
		if bytes.Equal(k, key) {
			return true
		}
	}
	return false
}