package cdp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

// decodeIncoming reads one CDP message in a single pass over the top-level
// object. Params and result stay raw. A message it cannot scan falls back
// to encoding/json.
func decodeIncoming(b []byte) (incoming, error) {
	msg, err := scanIncoming(b)
	if err == nil {
		return msg, nil
	}
	var fallback incoming
	jerr := json.Unmarshal(b, &fallback)
	if jerr != nil {
		return incoming{}, err
	}
	return fallback, nil
}

func scanIncoming(b []byte) (incoming, error) {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return incoming{}, errors.New("cdp: message is not an object")
	}
	i++
	var msg incoming
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return incoming{}, errors.New("cdp: truncated message")
		}
		if b[i] == '}' {
			break
		}
		key, next, err := parseString(b, i)
		if err != nil {
			return incoming{}, err
		}
		i = skipSpace(b, next)
		if i >= len(b) || b[i] != ':' {
			return incoming{}, errors.New("cdp: expected colon")
		}
		start := skipSpace(b, i+1)
		end, err := skipValue(b, start)
		if err != nil {
			return incoming{}, err
		}
		err = setField(&msg, key, b, start, b[start:end])
		if err != nil {
			return incoming{}, err
		}
		var done bool
		i, done, err = nextMember(b, end)
		if err != nil {
			return incoming{}, err
		}
		if done {
			break
		}
	}
	if skipSpace(b, i) != len(b) {
		return incoming{}, errors.New("cdp: trailing data")
	}
	return msg, nil
}

// setField stores one top-level member. start is the offset of its raw value
// and raw is the value's bytes.
func setField(msg *incoming, key string, b []byte, start int, raw []byte) error {
	switch key {
	case "id":
		n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
		if err != nil {
			return err
		}
		msg.ID = n
	case "sessionId":
		s, _, err := parseString(b, start)
		if err != nil {
			return err
		}
		msg.SessionID = s
	case "method":
		s, _, err := parseString(b, start)
		if err != nil {
			return err
		}
		msg.Method = s
	case "params":
		msg.Params = append([]byte(nil), raw...)
	case "result":
		msg.Result = append([]byte(nil), raw...)
	case "error":
		var cdpErr Error
		err := json.Unmarshal(raw, &cdpErr)
		if err != nil {
			return err
		}
		msg.Error = &cdpErr
	}
	return nil
}

// nextMember consumes the separator after a member value. done reports that
// the object closed.
func nextMember(b []byte, i int) (next int, done bool, err error) {
	i = skipSpace(b, i)
	if i >= len(b) {
		return 0, false, errors.New("cdp: truncated message")
	}
	switch b[i] {
	case ',':
		return i + 1, false, nil
	case '}':
		return i + 1, true, nil
	}
	return 0, false, errors.New("cdp: expected comma or end")
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

// simpleEscapes maps the one-byte JSON escapes to the byte they stand for.
var simpleEscapes = map[byte]byte{
	'"': '"', '\\': '\\', '/': '/',
	'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t',
}

func parseString(b []byte, i int) (string, int, error) {
	if i >= len(b) || b[i] != '"' {
		return "", 0, errors.New("cdp: expected string")
	}
	i++
	var buf []byte
	for i < len(b) {
		c := b[i]
		switch {
		case c == '"':
			return string(buf), i + 1, nil
		case c == '\\':
			var err error
			buf, i, err = appendEscape(buf, b, i)
			if err != nil {
				return "", 0, err
			}
		case c < 0x20:
			return "", 0, errors.New("cdp: control character in string")
		default:
			buf = append(buf, c)
			i++
		}
	}
	return "", 0, errors.New("cdp: truncated string")
}

// appendEscape decodes the escape that starts at b[i], a backslash. It returns
// the extended buffer and the offset after the escape.
func appendEscape(buf, b []byte, i int) ([]byte, int, error) {
	if i+1 >= len(b) {
		return nil, 0, errors.New("cdp: truncated escape")
	}
	if c, ok := simpleEscapes[b[i+1]]; ok {
		return append(buf, c), i + 2, nil
	}
	if b[i+1] != 'u' {
		return nil, 0, errors.New("cdp: bad escape")
	}
	if i+6 > len(b) {
		return nil, 0, errors.New("cdp: truncated unicode escape")
	}
	r, err := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
	if err != nil {
		return nil, 0, err
	}
	var tmp [utf8.UTFMax]byte
	n := utf8.EncodeRune(tmp[:], rune(r))
	return append(buf, tmp[:n]...), i + 6, nil
}

func skipValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errors.New("cdp: truncated value")
	}
	switch b[i] {
	case '"':
		_, end, err := parseString(b, i)
		return end, err
	case '{', '[':
		return skipContainer(b, i)
	case 't':
		return expectLit(b, i, "true")
	case 'f':
		return expectLit(b, i, "false")
	case 'n':
		return expectLit(b, i, "null")
	default:
		return skipNumber(b, i)
	}
}

func skipContainer(b []byte, i int) (int, error) {
	depth := 1
	i++
	for i < len(b) && depth > 0 {
		switch b[i] {
		case '"':
			_, next, err := parseString(b, i)
			if err != nil {
				return 0, err
			}
			i = next
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
		default:
			i++
		}
	}
	if depth != 0 {
		return 0, errors.New("cdp: truncated container")
	}
	return i, nil
}

func expectLit(b []byte, i int, lit string) (int, error) {
	if i+len(lit) > len(b) || string(b[i:i+len(lit)]) != lit {
		return 0, errors.New("cdp: expected " + lit)
	}
	return i + len(lit), nil
}

func skipNumber(b []byte, i int) (int, error) {
	start := i
	if b[i] == '-' {
		i++
	}
	for i < len(b) {
		c := b[i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			i++
			continue
		}
		break
	}
	if i == start || (i == start+1 && b[start] == '-') {
		return 0, errors.New("cdp: expected number")
	}
	return i, nil
}
