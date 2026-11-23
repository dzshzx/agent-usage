// 日志与接口 JSON 的宽松读取：按字段取值，取不出时返回写明字段与期望类型的行错误；按写入顺序输出键的 JSON 对象。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// decodeJSON 解析一段 JSON；数字保留原文（json.Number），按数取值时再转成 float64。
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, lineErr("不是合法的 JSON：%v", err)
	}
	return v, nil
}

// decodeObject 解析一行 JSON 并要求顶层是对象。
func decodeObject(data []byte) (map[string]any, error) {
	v, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, lineErr("顶层应为对象，实际是%s", jsonKind(v))
	}
	return m, nil
}

// jsonKind 是 JSON 值的类型名，写进行错误。
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "空值"
	case string:
		return "字符串"
	case bool:
		return "布尔值"
	case json.Number, float64:
		return "数字"
	case []any:
		return "数组"
	case map[string]any:
		return "对象"
	}
	return fmt.Sprintf("%T", v)
}

// wrongType 是字段 key 的值 v 不是期望类型 want 时的行错误。
func wrongType(key, want string, v any) error {
	return lineErr("字段 %s 应为%s，实际是%s", key, want, jsonKind(v))
}

// truthy 判断字段算不算有值：null、false、0、空串、空数组、空对象都算没有，按缺省处理。
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// objOrEmpty 取对象字段：没有值（见 truthy）时为空对象，有值却不是对象时报行错误。
func objOrEmpty(m map[string]any, key string) (map[string]any, error) {
	v := m[key]
	if !truthy(v) {
		return map[string]any{}, nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return nil, wrongType(key, "对象", v)
	}
	return o, nil
}

// strField 取可缺的字符串字段：缺失或 null 为 nil，其他类型报行错误。
func strField(m map[string]any, key string) (*string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, wrongType(key, "字符串", v)
	}
	return &s, nil
}

// strOr 取字符串字段，缺失或不是字符串时为空串。
func strOr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// requireStr 取必填的字符串字段。
func requireStr(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", lineErr("缺少字段 %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", wrongType(key, "字符串", v)
	}
	return s, nil
}

// number 按数取 JSON 值：数字照值（小数、指数写法都行），布尔记 1 与 0；其他类型取不出。
// 计数由调用方用 int64() 向零截断。
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// numOrZero 取计数字段：没有值（见 truthy）记 0，有值却取不出数时报行错误。
func numOrZero(m map[string]any, key string) (float64, error) {
	v := m[key]
	if !truthy(v) {
		return 0, nil
	}
	n, ok := number(v)
	if !ok {
		return 0, wrongType(key, "数字", v)
	}
	return n, nil
}

// numField 取可缺的数字字段：缺失或 null 时 present 为假。
func numField(m map[string]any, key string) (n float64, present bool, err error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	n, ok = number(v)
	if !ok {
		return 0, true, wrongType(key, "数字", v)
	}
	return n, true, nil
}

// requireNum 取必填的数字字段；值是数字字符串时也接受。
func requireNum(m map[string]any, key string) (float64, error) {
	v, ok := m[key]
	if !ok {
		return 0, lineErr("缺少字段 %s", key)
	}
	if s, isStr := v.(string); isStr {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, lineErr("字段 %s 的值 %q 不是数字", key, s)
		}
		return f, nil
	}
	n, ok := number(v)
	if !ok {
		return 0, wrongType(key, "数字", v)
	}
	return n, nil
}

// jsonText 是 JSON 值的文本：字符串原样，其他值取紧凑 JSON 写法（数字保留原文，null 写作 null）。
func jsonText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// orderedObject 是按写入顺序输出键的 JSON 对象，用在键序有含义的地方：按日趋势里各模型按当天金额
// 从高到低（DayReport.Models）；来源块的字段按固定顺序，部分字段随子命令才出现（SourceBlock）。
type orderedObject struct {
	keys []string
	vals map[string]any
}

func newOrderedObject() *orderedObject { return &orderedObject{vals: map[string]any{}} }

// Set 写入键值；键已存在时只换值，位置不变。
func (m *orderedObject) Set(key string, v any) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = v
}

func (m *orderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalNoEscape(m.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalNoEscape 编码一个值，不转义 <、>、&（与整份输出的 marshalIndent 一致）。
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
