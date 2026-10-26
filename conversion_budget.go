package main

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"unicode/utf8"
)

// Converted requests share the ingress size limit. Check before marshaling:
// json.Encoder also materializes the whole value before calling its writer.
type conversionSizeError struct{ limit int }

func (e *conversionSizeError) Error() string {
	return fmt.Sprintf("协议转换后的请求超过 %d 字节限制", e.limit)
}

func marshalConvertedRequest(value any) ([]byte, error) {
	if _, err := measureConversionJSON(value, requestDecodedLimit); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// quoted is the encoded value's size when embedded inside another JSON string,
// excluding that string's outer quotes. It lets reference expansion account for
// both schema serialization and its subsequent escaping without allocating it.
type conversionJSONSize struct{ plain, quoted int }

// Go's legacy and newer JSON implementations differ in how they replace an
// invalid UTF-8 byte. Calibrate only these tiny fixed values; preflight never
// marshals an unbounded caller string or raw value to discover its size.
var (
	conversionInvalidStringSize = invalidConversionRuneSize("\xff")
	conversionInvalidRawSize    = invalidConversionRuneSize(json.RawMessage{'"', 0xff, '"'})
)

func invalidConversionRuneSize(value any) conversionJSONSize {
	encoded, _ := json.Marshal(value)
	quoted, _ := json.Marshal(string(encoded))
	return conversionJSONSize{plain: len(encoded) - 2, quoted: len(quoted) - 6}
}

type conversionJSONCounter struct {
	conversionJSONSize
	limit int
}

func measureConversionJSON(value any, limit int) (conversionJSONSize, error) {
	counter := conversionJSONCounter{limit: limit}
	err := counter.value(value, 0)
	return counter.conversionJSONSize, err
}

func (c *conversionJSONCounter) add(plain, quoted int) error {
	if plain > c.limit-c.plain {
		return &conversionSizeError{limit: c.limit}
	}
	c.plain += plain
	c.quoted += quoted
	return nil
}

func (c *conversionJSONCounter) literal(text string) error {
	quoted := len(text)
	for i := 0; i < len(text); i++ {
		if text[i] == '"' || text[i] == '\\' {
			quoted++
		}
	}
	return c.add(len(text), quoted)
}

func (c *conversionJSONCounter) string(text string) error {
	if err := c.add(2, 4); err != nil {
		return err
	}
	for i := 0; i < len(text); {
		b := text[i]
		switch {
		case b == '"' || b == '\\':
			if err := c.add(2, 4); err != nil {
				return err
			}
			i++
		case b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
			if err := c.add(2, 3); err != nil {
				return err
			}
			i++
		case b < 0x20 || b == '<' || b == '>' || b == '&':
			if err := c.add(6, 7); err != nil {
				return err
			}
			i++
		case b < utf8.RuneSelf:
			start, end := i, len(text)
			// Bound the scan by the remaining budget too: huge rejected ASCII
			// values must not be scanned to the end just to discover overflow.
			if remaining := c.limit - c.plain; remaining < end-i {
				end = i + remaining + 1
			}
			for i < end && conversionSafeASCII(text[i]) {
				i++
			}
			if err := c.add(i-start, i-start); err != nil {
				return err
			}
		default:
			r, size := utf8.DecodeRuneInString(text[i:])
			plain, quoted := size, size
			if r == '\u2028' || r == '\u2029' {
				plain, quoted = 6, 7
			} else if r == utf8.RuneError && size == 1 {
				plain, quoted = conversionInvalidStringSize.plain, conversionInvalidStringSize.quoted
			}
			if err := c.add(plain, quoted); err != nil {
				return err
			}
			i += size
		}
	}
	return nil
}

// The hot scan uses one byte classification lookup. Escaping and UTF-8 still
// follow string's original slow path and exact plain/quoted budget accounting.
var conversionSafeASCIIBytes = func() [256]bool {
	var safe [256]bool
	for b := 0x20; b < utf8.RuneSelf; b++ {
		safe[b] = b != '"' && b != '\\' && b != '<' && b != '>' && b != '&'
	}
	return safe
}()

func conversionSafeASCII(b byte) bool { return conversionSafeASCIIBytes[b] }

func (c *conversionJSONCounter) value(value any, depth int) error {
	// This matches encoding/json's nesting limit and also terminates cycles from
	// direct converter callers; decoded HTTP requests cannot contain cycles.
	if depth > 10000 {
		return fmt.Errorf("协议转换后的 JSON 嵌套过深或存在循环引用")
	}
	switch v := value.(type) {
	case nil:
		return c.literal("null")
	case string:
		return c.string(v)
	case bool:
		if v {
			return c.literal("true")
		}
		return c.literal("false")
	case json.Number:
		number := string(v)
		if number == "" {
			number = "0" // encoding/json's zero json.Number behavior.
		}
		if !validConversionJSONNumber(number) {
			return fmt.Errorf("协议转换包含无效 JSON 数字")
		}
		return c.add(len(number), len(number))
	case json.RawMessage:
		return c.raw(v)
	case map[string]any:
		if v == nil {
			return c.literal("null")
		}
		if err := c.literal("{"); err != nil {
			return err
		}
		first := true
		for key, element := range v {
			if !first {
				if err := c.literal(","); err != nil {
					return err
				}
			}
			first = false
			if err := c.string(key); err != nil {
				return err
			}
			if err := c.literal(":"); err != nil {
				return err
			}
			if err := c.value(element, depth+1); err != nil {
				return err
			}
		}
		return c.literal("}")
	}

	// Request maps only contain JSON values. Custom marshalers can materialize
	// arbitrary output behind an otherwise small value, so do not invoke them
	// during a bounded preflight.
	if _, custom := value.(json.Marshaler); custom {
		return fmt.Errorf("协议转换包含不支持的自定义 JSON 类型 %T", value)
	}
	if _, custom := value.(encoding.TextMarshaler); custom {
		return fmt.Errorf("协议转换包含不支持的自定义 JSON 类型 %T", value)
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return c.literal("null")
		}
		return c.value(rv.Elem().Interface(), depth+1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var buf [32]byte
		encoded := strconv.AppendInt(buf[:0], rv.Int(), 10)
		return c.add(len(encoded), len(encoded))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var buf [32]byte
		encoded := strconv.AppendUint(buf[:0], rv.Uint(), 10)
		return c.add(len(encoded), len(encoded))
	case reflect.Float32, reflect.Float64:
		number := rv.Float()
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return fmt.Errorf("协议转换包含非有限 JSON 数字")
		}
		// A floating point scalar is bounded to a few dozen bytes. Let json
		// choose exactly the same exponent representation as the final marshal.
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return c.add(len(encoded), len(encoded))
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return c.literal("null")
		}
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			size := base64.StdEncoding.EncodedLen(rv.Len())
			return c.add(size+2, size+4)
		}
		if err := c.literal("["); err != nil {
			return err
		}
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				if err := c.literal(","); err != nil {
					return err
				}
			}
			if err := c.value(rv.Index(i).Interface(), depth+1); err != nil {
				return err
			}
		}
		return c.literal("]")
	case reflect.Map:
		if rv.IsNil() {
			return c.literal("null")
		}
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("协议转换包含不支持的 JSON 对象键")
		}
		if rv.Type().Key().Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
			return fmt.Errorf("协议转换包含不支持的自定义 JSON 对象键")
		}
		if err := c.literal("{"); err != nil {
			return err
		}
		iter, first := rv.MapRange(), true
		for iter.Next() {
			if !first {
				if err := c.literal(","); err != nil {
					return err
				}
			}
			first = false
			if err := c.string(iter.Key().String()); err != nil {
				return err
			}
			if err := c.literal(":"); err != nil {
				return err
			}
			if err := c.value(iter.Value().Interface(), depth+1); err != nil {
				return err
			}
		}
		return c.literal("}")
	case reflect.String:
		return c.string(rv.String())
	case reflect.Bool:
		if rv.Bool() {
			return c.literal("true")
		}
		return c.literal("false")
	default:
		return fmt.Errorf("协议转换包含不支持的 JSON 类型 %T", value)
	}
}

func (c *conversionJSONCounter) raw(raw json.RawMessage) error {
	if raw == nil {
		return c.literal("null")
	}
	if !json.Valid(raw) {
		return fmt.Errorf("协议转换包含无效 JSON 原文")
	}
	inside := false
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if !inside && (b == ' ' || b == '\n' || b == '\r' || b == '\t') {
			continue
		}
		if b == '"' {
			inside = !inside
		}
		if inside && b == '\\' {
			if err := c.literal(string(raw[i : i+2])); err != nil {
				return err
			}
			i++
			continue
		}
		if inside && (b == '<' || b == '>' || b == '&') {
			if err := c.add(6, 7); err != nil {
				return err
			}
			continue
		}
		if inside && b == 0xe2 && i+2 < len(raw) && raw[i+1] == 0x80 && (raw[i+2] == 0xa8 || raw[i+2] == 0xa9) {
			if err := c.add(6, 7); err != nil {
				return err
			}
			i += 2
			continue
		}
		if inside && b >= utf8.RuneSelf {
			r, size := utf8.DecodeRune(raw[i:])
			if r == utf8.RuneError && size == 1 {
				if err := c.add(conversionInvalidRawSize.plain, conversionInvalidRawSize.quoted); err != nil {
					return err
				}
			} else {
				if err := c.add(size, size); err != nil {
					return err
				}
				i += size - 1
			}
			continue
		}
		plain, quoted := 1, 1
		if b == '"' || b == '\\' {
			quoted++
		}
		if err := c.add(plain, quoted); err != nil {
			return err
		}
	}
	return nil
}

func validConversionJSONNumber(number string) bool {
	i := 0
	if i < len(number) && number[i] == '-' {
		i++
	}
	if i == len(number) {
		return false
	}
	if number[i] == '0' {
		i++
	} else {
		if number[i] < '1' || number[i] > '9' {
			return false
		}
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
	}
	if i < len(number) && number[i] == '.' {
		i++
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(number) && (number[i] == 'e' || number[i] == 'E') {
		i++
		if i < len(number) && (number[i] == '+' || number[i] == '-') {
			i++
		}
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(number)
}
