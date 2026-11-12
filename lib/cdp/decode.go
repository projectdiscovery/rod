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
	if jerr := json.Unmarshal(b, &fallback); jerr != nil {
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
			i++
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
		i = skipSpace(b, i+1)
		start := i
		end, err := skipValue(b, i)
		if err != nil {
			return incoming{}, err
		}
		raw := b[start:end]
		switch key {
		case "id":
			n, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
			if err != nil {
				return incoming{}, err
			}
			msg.ID = n
		case "sessionId":
			s, _, err := parseString(b, start)
			if err != nil {
				return incoming{}, err
			}
			msg.SessionID = s
		case "method":
			s, _, err := parseString(b, start)
			if err != nil {
				return incoming{}, err
			}
			msg.Method = s
		case "params":
			msg.Params = append([]byte(nil), raw...)
		case "result":
			msg.Result = append([]byte(nil), raw...)
		case "error":
			var cdpErr Error
			if err := json.Unmarshal(raw, &cdpErr); err != nil {
				return incoming{}, err
			}
			msg.Error = &cdpErr
		}
		i = skipSpace(b, end)
		if i >= len(b) {
			return incoming{}, errors.New("cdp: truncated message")
		}
		if b[i] == ',' {
			i++
			continue
		}
		if b[i] == '}' {
			i++
			break
		}
		return incoming{}, errors.New("cdp: expected comma or end")
	}
	if skipSpace(b, i) != len(b) {
		return incoming{}, errors.New("cdp: trailing data")
	}
	return msg, nil
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

func parseString(b []byte, i int) (string, int, error) {
	if i >= len(b) || b[i] != '"' {
		return "", 0, errors.New("cdp: expected string")
	}
	i++
	var buf []byte
	for i < len(b) {
		c := b[i]
		if c == '"' {
			return string(buf), i + 1, nil
		}
		if c == '\\' {
			if i+1 >= len(b) {
				return "", 0, errors.New("cdp: truncated escape")
			}
			switch b[i+1] {
			case '"', '\\', '/':
				buf = append(buf, b[i+1])
				i += 2
			case 'b':
				buf = append(buf, '\b')
				i += 2
			case 'f':
				buf = append(buf, '\f')
				i += 2
			case 'n':
				buf = append(buf, '\n')
				i += 2
			case 'r':
				buf = append(buf, '\r')
				i += 2
			case 't':
				buf = append(buf, '\t')
				i += 2
			case 'u':
				if i+6 > len(b) {
					return "", 0, errors.New("cdp: truncated unicode escape")
				}
				r, err := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
				if err != nil {
					return "", 0, err
				}
				var tmp [utf8.UTFMax]byte
				n := utf8.EncodeRune(tmp[:], rune(r))
				buf = append(buf, tmp[:n]...)
				i += 6
			default:
				return "", 0, errors.New("cdp: bad escape")
			}
			continue
		}
		if c < 0x20 {
			return "", 0, errors.New("cdp: control character in string")
		}
		buf = append(buf, c)
		i++
	}
	return "", 0, errors.New("cdp: truncated string")
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
