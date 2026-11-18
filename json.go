// JSON value parsing and hand-rolled JSON writing.
//
// No encoding/json: the hot paths (issue reads, advertisement-adjacent
// shapes) build responses with zero reflection and minimal allocation.
// Parsed bodies use JVal: nil, bool, int64, float64, string, []JVal,
// map[string]JVal.

package main

import (
	"strconv"
	"unicode/utf8"
)

// JVal is a parsed JSON value.
type JVal any

func jGet(obj JVal, key string) (JVal, bool) {
	m, ok := obj.(map[string]JVal)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

func jStr(v JVal) string {
	s, _ := v.(string)
	return s
}

func jInt(v JVal) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

func jBool(v JVal) bool {
	b, _ := v.(bool)
	return b
}

// --- parser ---

type jParser struct {
	b []byte
	i int
}

func parseJSON(b []byte) (JVal, error) {
	p := &jParser{b: b}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, errJSON("unexpected trailing input")
	}
	return v, nil
}

type jsonError string

func (e jsonError) Error() string { return string(e) }
func errJSON(s string) error     { return jsonError(s) }

func (p *jParser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jParser) value() (JVal, error) {
	if p.i >= len(p.b) {
		return nil, errJSON("unexpected end of input")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, err := p.str()
		return s, err
	case c == 't':
		if p.lit("true") {
			return true, nil
		}
		return nil, errJSON("invalid literal")
	case c == 'f':
		if p.lit("false") {
			return false, nil
		}
		return nil, errJSON("invalid literal")
	case c == 'n':
		if p.lit("null") {
			return nil, nil
		}
		return nil, errJSON("invalid literal")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, errJSON("unexpected character")
	}
}

func (p *jParser) lit(s string) bool {
	if len(p.b)-p.i < len(s) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jParser) number() (JVal, error) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
	}
	isFloat := false
	if p.i < len(p.b) && p.b[p.i] == '.' {
		isFloat = true
		p.i++
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		isFloat = true
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	text := string(p.b[start:p.i])
	if !isFloat {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, errJSON("invalid number")
	}
	return f, nil
}

func (p *jParser) str() (string, error) {
	// p.b[p.i] == '"'
	p.i++
	var out []byte
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return string(out), nil
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", errJSON("unterminated escape")
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			case '/':
				out = append(out, '/')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r, err := p.hex4()
				if err != nil {
					return "", err
				}
				if r >= 0xD800 && r <= 0xDBFF {
					// high surrogate: expect a low surrogate
					if p.i+1 < len(p.b) && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
						save := p.i
						p.i += 2
						r2, err := p.hex4()
						if err == nil && r2 >= 0xDC00 && r2 <= 0xDFFF {
							r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
						} else {
							p.i = save
							r = 0xFFFD
						}
					} else {
						r = 0xFFFD
					}
				} else if r >= 0xDC00 && r <= 0xDFFF {
					r = 0xFFFD
				}
				out = utf8.AppendRune(out, r)
			default:
				return "", errJSON("invalid escape")
			}
			continue
		}
		if c < 0x20 {
			return "", errJSON("unescaped control character")
		}
		out = append(out, c)
		p.i++
	}
	return "", errJSON("unterminated string")
}

func (p *jParser) hex4() (rune, error) {
	if p.i+4 > len(p.b) {
		return 0, errJSON("bad \\u escape")
	}
	n, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, errJSON("bad \\u escape")
	}
	p.i += 4
	return rune(n), nil
}

func (p *jParser) object() (JVal, error) {
	p.i++ // '{'
	m := make(map[string]JVal)
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return m, nil
	}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, errJSON("expected object key")
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, errJSON("expected ':'")
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		m[k] = v
		p.ws()
		if p.i >= len(p.b) {
			return nil, errJSON("unterminated object")
		}
		if p.b[p.i] == '}' {
			p.i++
			return m, nil
		}
		if p.b[p.i] != ',' {
			return nil, errJSON("expected ',' in object")
		}
		p.i++
	}
}

func (p *jParser) array() (JVal, error) {
	p.i++ // '['
	a := []JVal{}
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return a, nil
	}
	for {
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		p.ws()
		if p.i >= len(p.b) {
			return nil, errJSON("unterminated array")
		}
		if p.b[p.i] == ']' {
			p.i++
			return a, nil
		}
		if p.b[p.i] != ',' {
			return nil, errJSON("expected ',' in array")
		}
		p.i++
	}
}

// --- writer ---

// appendJSONString appends s as a JSON string literal (with quotes).
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0')
				dst = append(dst, hexDigit(c>>4), hexDigit(c&0x0f))
			}
			start = i + 1
		}
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}

// appendJSONVal appends a parsed JVal as JSON (used for id passthrough etc.).
func appendJSONVal(dst []byte, v JVal) []byte {
	switch n := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if n {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int64:
		return strconv.AppendInt(dst, n, 10)
	case float64:
		return strconv.AppendFloat(dst, n, 'g', -1, 64)
	case string:
		return appendJSONString(dst, n)
	case []JVal:
		dst = append(dst, '[')
		for i, e := range n {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONVal(dst, e)
		}
		return append(dst, ']')
	case map[string]JVal:
		dst = append(dst, '{')
		i := 0
		for k, e := range n {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSONVal(dst, e)
			i++
		}
		return append(dst, '}')
	}
	return append(dst, "null"...)
}

func jsonQuote(s string) string {
	return string(appendJSONString(nil, s))
}
