package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// A config set (#63) edits the file in place: the value at a JSON
// pointer is replaced, inserted or deleted in the file's own text, so
// every other line stays as the owner wrote it and the diff the owner
// approves is the change alone. The edit is checked against the same
// change made on the decoded document: a text whose meaning is not
// exactly that is refused.

// jsonNode is a value of a JSON text and where it lies in it.
type jsonNode struct {
	start, end int
	kind       byte // '{', '[' or 'v' for any other value
	members    []jsonMember
	elems      []*jsonNode
}

// jsonMember is an object's member: its key, where the key starts, and
// its value.
type jsonMember struct {
	key      string
	keyStart int
	val      *jsonNode
}

// jsonScanner reads a JSON text's values with their places.
type jsonScanner struct {
	b []byte
	i int
}

func (s *jsonScanner) space() {
	for s.i < len(s.b) && (s.b[s.i] == ' ' || s.b[s.i] == '\t' || s.b[s.i] == '\n' || s.b[s.i] == '\r') {
		s.i++
	}
}

func (s *jsonScanner) fail(what string) error {
	return fmt.Errorf("the file is not JSON at byte %d: %s", s.i, what)
}

func (s *jsonScanner) str() (string, error) {
	start := s.i
	if s.i >= len(s.b) || s.b[s.i] != '"' {
		return "", s.fail("a string was expected")
	}
	s.i++
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case '\\':
			s.i += 2
			continue
		case '"':
			s.i++
			var out string
			if err := json.Unmarshal(s.b[start:s.i], &out); err != nil {
				return "", s.fail(err.Error())
			}
			return out, nil
		}
		s.i++
	}
	return "", s.fail("a string does not end")
}

func (s *jsonScanner) value() (*jsonNode, error) {
	s.space()
	if s.i >= len(s.b) {
		return nil, s.fail("a value was expected")
	}
	n := &jsonNode{start: s.i, kind: 'v'}
	switch c := s.b[s.i]; c {
	case '{':
		n.kind = '{'
		s.i++
		s.space()
		if s.i < len(s.b) && s.b[s.i] == '}' {
			s.i++
			break
		}
		for {
			s.space()
			m := jsonMember{keyStart: s.i}
			key, err := s.str()
			if err != nil {
				return nil, err
			}
			m.key = key
			s.space()
			if s.i >= len(s.b) || s.b[s.i] != ':' {
				return nil, s.fail("a colon was expected")
			}
			s.i++
			if m.val, err = s.value(); err != nil {
				return nil, err
			}
			n.members = append(n.members, m)
			s.space()
			if s.i < len(s.b) && s.b[s.i] == ',' {
				s.i++
				continue
			}
			if s.i < len(s.b) && s.b[s.i] == '}' {
				s.i++
				break
			}
			return nil, s.fail("a comma or a closing brace was expected")
		}
	case '[':
		n.kind = '['
		s.i++
		s.space()
		if s.i < len(s.b) && s.b[s.i] == ']' {
			s.i++
			break
		}
		for {
			v, err := s.value()
			if err != nil {
				return nil, err
			}
			n.elems = append(n.elems, v)
			s.space()
			if s.i < len(s.b) && s.b[s.i] == ',' {
				s.i++
				continue
			}
			if s.i < len(s.b) && s.b[s.i] == ']' {
				s.i++
				break
			}
			return nil, s.fail("a comma or a closing bracket was expected")
		}
	case '"':
		if _, err := s.str(); err != nil {
			return nil, err
		}
	default:
		for s.i < len(s.b) && !strings.ContainsRune(" \t\r\n,]}", rune(s.b[s.i])) {
			s.i++
		}
		var v any
		if err := json.Unmarshal(s.b[n.start:s.i], &v); err != nil {
			return nil, s.fail(err.Error())
		}
	}
	n.end = s.i
	return n, nil
}

// pointerTokens are a JSON pointer's reference tokens, unescaped.
func pointerTokens(ptr string) ([]string, error) {
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("pointer %q does not begin with /; the whole file is not set at once", cutUTF8(ptr, 80))
	}
	toks := strings.Split(ptr[1:], "/")
	for i, t := range toks {
		toks[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return toks, nil
}

// lineIndent is the whitespace that begins the line holding byte i.
func lineIndent(b []byte, i int) string {
	start := bytes.LastIndexByte(b[:i], '\n') + 1
	j := start
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	return string(b[start:j])
}

// indentUnit is the file's indent: that of its first indented line, else
// two spaces.
func indentUnit(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimLeft(l, " \t")
		if t != "" && len(t) < len(l) {
			return l[:len(l)-len(t)]
		}
	}
	return "  "
}

// indented is the JSON value raw laid out at prefix, its keys in the
// order they came.
func indented(raw json.RawMessage, prefix, unit string) (string, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, unit); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// nestedValue is value wrapped in an object for each of keys, outermost first.
func nestedValue(keys []string, value json.RawMessage) json.RawMessage {
	out := []byte(value)
	for i := len(keys) - 1; i >= 0; i-- {
		k, _ := json.Marshal(keys[i])
		out = []byte("{" + string(k) + ":" + string(out) + "}")
	}
	return out
}

// editJSON is the JSON text src with value set at ptr, or the value there
// deleted, as text: other lines are untouched. A set reaches into
// members that are not there yet by creating them; an array takes an
// index it has, or "-" to append.
func editJSON(src []byte, ptr string, value json.RawMessage, del bool) ([]byte, error) {
	toks, err := pointerTokens(ptr)
	if err != nil {
		return nil, err
	}
	if !del {
		var v any
		if err := json.Unmarshal(value, &v); err != nil {
			return nil, fmt.Errorf("the value is not JSON: %v", err)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, value); err != nil {
			return nil, err
		}
		value = buf.Bytes()
	}
	sc := &jsonScanner{b: src}
	root, err := sc.value()
	if err != nil {
		return nil, err
	}
	if sc.space(); sc.i != len(src) {
		return nil, sc.fail("text follows the document")
	}
	unit := indentUnit(src)
	splice := func(from, to int, text string) []byte {
		return append(append(append([]byte{}, src[:from]...), text...), src[to:]...)
	}
	node := root
	for depth, tok := range toks {
		last := depth == len(toks)-1
		switch node.kind {
		case '{':
			var m *jsonMember
			idx := -1
			for i := range node.members {
				if node.members[i].key == tok {
					m, idx = &node.members[i], i
				}
			}
			if m == nil {
				if del {
					return nil, fmt.Errorf("%s: there is no %q to delete", ptr, tok)
				}
				k, _ := json.Marshal(tok)
				v := nestedValue(toks[depth+1:], value)
				if len(node.members) == 0 {
					outer := lineIndent(src, node.start)
					text, err := indented(v, outer+unit, unit)
					if err != nil {
						return nil, err
					}
					return splice(node.start, node.end, "{\n"+outer+unit+string(k)+": "+text+"\n"+outer+"}"), nil
				}
				lastM := node.members[len(node.members)-1]
				inner := lineIndent(src, lastM.keyStart)
				text, err := indented(v, inner, unit)
				if err != nil {
					return nil, err
				}
				return splice(lastM.val.end, lastM.val.end, ",\n"+inner+string(k)+": "+text), nil
			}
			if !last {
				node = m.val
				continue
			}
			if del {
				switch {
				case len(node.members) == 1:
					return splice(node.start, node.end, "{}"), nil
				case idx == 0:
					return splice(m.keyStart, node.members[1].keyStart, ""), nil
				default:
					return splice(node.members[idx-1].val.end, m.val.end, ""), nil
				}
			}
			text, err := indented(value, lineIndent(src, m.val.start), unit)
			if err != nil {
				return nil, err
			}
			return splice(m.val.start, m.val.end, text), nil
		case '[':
			if tok == "-" {
				if del || !last {
					return nil, fmt.Errorf("%s: - only appends a value", ptr)
				}
				if len(node.elems) == 0 {
					outer := lineIndent(src, node.start)
					text, err := indented(value, outer+unit, unit)
					if err != nil {
						return nil, err
					}
					return splice(node.start, node.end, "[\n"+outer+unit+text+"\n"+outer+"]"), nil
				}
				lastE := node.elems[len(node.elems)-1]
				sep := ", "
				prefix := lineIndent(src, lastE.start)
				if bytes.Contains(src[node.start:node.end], []byte("\n")) {
					sep = ",\n" + prefix
				}
				text, err := indented(value, prefix, unit)
				if err != nil {
					return nil, err
				}
				return splice(lastE.end, lastE.end, sep+text), nil
			}
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node.elems) || tok != strconv.Itoa(i) {
				return nil, fmt.Errorf("%s: %q is not an index the array has (0 to %d), or -", ptr, tok, len(node.elems)-1)
			}
			if !last {
				node = node.elems[i]
				continue
			}
			if del {
				switch {
				case len(node.elems) == 1:
					return splice(node.start, node.end, "[]"), nil
				case i == 0:
					return splice(node.elems[0].start, node.elems[1].start, ""), nil
				default:
					return splice(node.elems[i-1].end, node.elems[i].end, ""), nil
				}
			}
			text, err := indented(value, lineIndent(src, node.elems[i].start), unit)
			if err != nil {
				return nil, err
			}
			return splice(node.elems[i].start, node.elems[i].end, text), nil
		default:
			return nil, fmt.Errorf("%s: %q is inside a value that is neither an object nor an array", ptr, tok)
		}
	}
	return nil, fmt.Errorf("%s: nothing to set", ptr)
}

// pointerSet is doc with the change made on its decoded form: the meaning
// the text edit must have.
func pointerSet(doc any, toks []string, value any, del bool) (any, error) {
	if len(toks) == 0 {
		return value, nil
	}
	tok := toks[0]
	switch x := doc.(type) {
	case map[string]any:
		child, ok := x[tok]
		if len(toks) == 1 && del {
			if !ok {
				return nil, errors.New("no such member")
			}
			delete(x, tok)
			return x, nil
		}
		if !ok {
			child = map[string]any{}
			if len(toks) == 1 {
				child = nil
			}
		}
		v, err := pointerSet(child, toks[1:], value, del)
		if err != nil {
			return nil, err
		}
		x[tok] = v
		return x, nil
	case []any:
		if tok == "-" {
			return append(x, value), nil
		}
		i, err := strconv.Atoi(tok)
		if err != nil || i < 0 || i >= len(x) {
			return nil, errors.New("no such index")
		}
		if len(toks) == 1 && del {
			return append(x[:i:i], x[i+1:]...), nil
		}
		v, err := pointerSet(x[i], toks[1:], value, del)
		if err != nil {
			return nil, err
		}
		x[i] = v
		return x, nil
	case nil:
		if del {
			return nil, errors.New("no such member")
		}
		return pointerSet(map[string]any{}, toks, value, del)
	}
	return nil, errors.New("not an object or an array")
}

// decodeNumbers decodes b keeping its numbers as they were written.
func decodeNumbers(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// setConfigText is src with the change made, checked: the edited text
// means src's document with that one change, and nothing else.
func setConfigText(src []byte, ptr string, value json.RawMessage, del bool) ([]byte, error) {
	out, err := editJSON(src, ptr, value, del)
	if err != nil {
		return nil, err
	}
	toks, _ := pointerTokens(ptr)
	want, err := decodeNumbers(src)
	if err != nil {
		return nil, err
	}
	var v any
	if !del {
		if v, err = decodeNumbers(value); err != nil {
			return nil, err
		}
	}
	if want, err = pointerSet(want, toks, v, del); err != nil {
		return nil, fmt.Errorf("%s: %v", ptr, err)
	}
	got, err := decodeNumbers(out)
	if err != nil || !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("%s: the edit of the file's text does not mean the change; edit this one by hand", ptr)
	}
	return out, nil
}

// diffLines is a unified diff of a and b with three lines of context,
// named name, or "" when they are the same. Files of more lines than
// diffMax are refused: a config is far smaller.
func diffLines(name string, a, b []byte) (string, error) {
	al, bl := splitLines(a), splitLines(b)
	if len(al) > diffMax || len(bl) > diffMax {
		return "", fmt.Errorf("the file has more than %d lines", diffMax)
	}
	// lcs[i][j] is the longest common run of al[i:] and bl[j:].
	lcs := make([][]int, len(al)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type line struct {
		op   byte
		text string
		a, b int
	}
	var ops []line
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && al[i] == bl[j]:
			ops = append(ops, line{' ', al[i], i, j})
			i, j = i+1, j+1
		case i < len(al) && (j == len(bl) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, line{'-', al[i], i, j})
			i++
		default:
			ops = append(ops, line{'+', bl[j], i, j})
			j++
		}
	}
	const context = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", name, name)
	changed := false
	for k := 0; k < len(ops); {
		if ops[k].op == ' ' {
			k++
			continue
		}
		changed = true
		start := max(k-context, 0)
		end := k
		for end < len(ops) {
			if ops[end].op != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].op == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		na, nb := 0, 0
		for _, l := range ops[start:end] {
			if l.op != '+' {
				na++
			}
			if l.op != '-' {
				nb++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ops[start].a+1, na, ops[start].b+1, nb)
		for _, l := range ops[start:end] {
			out.WriteByte(l.op)
			out.WriteString(l.text)
			out.WriteByte('\n')
		}
		k = end
	}
	if !changed {
		return "", nil
	}
	return out.String(), nil
}

// diffMax bounds the lines of a file diffLines compares.
const diffMax = 5000

func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
