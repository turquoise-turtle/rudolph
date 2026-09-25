package request

import (
	"bytes"
	"encoding/json"
	"errors"
)

// errInvalidJSON is returned for a body that is not a single, well-formed JSON document.
var errInvalidJSON = errors.New("request body is not valid JSON")

// normalizeProtoJSONKeys rewrites every lowerCamelCase object key in a JSON document to snake_case. Keys that
// are already snake_case are left alone, so both spellings of a protobuf JSON field are accepted.
//
// A body that spells the same field both ways resolves the same way every time: within an object the key
// that is already snake_case wins, wherever it appears, and among keys of the same spelling the last one wins,
// as it does in encoding/json. Only keys are rewritten; values are copied byte for byte, and the document keeps
// its order.
//
// The document is validated once and then scanned as bytes, without decoding values into Go values, so the
// cost stays close to one extra copy of the body; request bodies can be several MiB (see MaxBodySize). Only
// escaped keys and objects that spell a field both ways - neither of which Santa produces - allocate more.
func normalizeProtoJSONKeys(data []byte) ([]byte, error) {
	if !json.Valid(data) {
		return nil, errInvalidJSON
	}

	survey := keySurvey{data: data}
	survey.value(skipSpace(data, 0))
	if !survey.camelCase {
		// Nothing is spelled camelCase, so the body is already in the spelling Rudolph's types use.
		return data, nil
	}

	rewriter := keyRewriter{
		data:       data,
		out:        make([]byte, 0, len(data)+survey.growth),
		collisions: survey.collisions,
	}
	rewriter.value(skipSpace(data, 0))
	return rewriter.out, nil
}

// keySurvey is the first pass: it finds out whether any key needs rewriting at all, and which objects spell
// a field both ways. Objects are numbered in the order their opening brace appears, which the second pass
// reproduces.
type keySurvey struct {
	data      []byte
	camelCase bool
	objects   int
	// growth is how many bytes rewriting the keys can add (one "_" per word boundary), so the second pass
	// can size its output once instead of growing it.
	growth int
	// collisions maps the number of an object that spells a field both ways to its snake_case keys.
	collisions map[int]map[string]bool
	// keys holds, per nesting depth, the keys of the object being scanned at that depth. The slices are
	// reused and point into data.
	keys  [][][]byte
	depth int
}

// value scans the value starting at i and returns the index just after it.
func (s *keySurvey) value(i int) int {
	switch s.data[i] {
	case '{':
		return s.object(i)
	case '[':
		i = skipSpace(s.data, i+1)
		for s.data[i] != ']' {
			i = skipSpace(s.data, s.value(i))
			if s.data[i] == ',' {
				i = skipSpace(s.data, i+1)
			}
		}
		return i + 1
	default:
		return scalarEnd(s.data, i)
	}
}

func (s *keySurvey) object(i int) int {
	index := s.objects
	s.objects++
	if len(s.keys) <= s.depth {
		s.keys = append(s.keys, nil)
	}
	keys := s.keys[s.depth][:0]
	s.depth++

	hasCamelCase := false
	i = skipSpace(s.data, i+1)
	for s.data[i] != '}' {
		end := stringEnd(s.data, i)
		key := keyText(s.data[i:end])
		if hasUpperASCII(key) {
			hasCamelCase = true
			s.growth += wordBoundaries(key)
			if bytes.IndexByte(s.data[i:end], '\\') >= 0 {
				// A decoded key is re-encoded, which can take more room than it did; allow for that.
				s.growth += end - i
			}
		}
		keys = append(keys, key)

		i = skipSpace(s.data, end) // at ':'
		i = skipSpace(s.data, i+1) // at the value
		i = skipSpace(s.data, s.value(i))
		if s.data[i] == ',' {
			i = skipSpace(s.data, i+1)
		}
	}

	s.depth--
	s.keys[s.depth] = keys
	if hasCamelCase {
		s.camelCase = true
		if hasCollision(keys) {
			if s.collisions == nil {
				s.collisions = map[int]map[string]bool{}
			}
			s.collisions[index] = snakeCaseKeys(keys)
		}
	}
	return i + 1
}

// hasCollision reports whether a camelCase key and a snake_case key in one object name the same field.
func hasCollision(keys [][]byte) bool {
	for _, camel := range keys {
		if !hasUpperASCII(camel) {
			continue
		}
		for _, snake := range keys {
			if !hasUpperASCII(snake) && snakeCaseEquals(camel, snake) {
				return true
			}
		}
	}
	return false
}

func snakeCaseKeys(keys [][]byte) map[string]bool {
	snakeKeys := map[string]bool{}
	for _, key := range keys {
		if !hasUpperASCII(key) {
			snakeKeys[string(key)] = true
		}
	}
	return snakeKeys
}

// keyRewriter is the second pass: it copies the document, rewriting keys and dropping the camelCase member
// of every field that is also spelled snake_case in the same object.
type keyRewriter struct {
	data       []byte
	out        []byte
	objects    int
	collisions map[int]map[string]bool
}

func (r *keyRewriter) value(i int) int {
	switch r.data[i] {
	case '{':
		return r.object(i)
	case '[':
		r.out = append(r.out, '[')
		i = skipSpace(r.data, i+1)
		first := true
		for r.data[i] != ']' {
			if !first {
				r.out = append(r.out, ',')
			}
			first = false
			i = skipSpace(r.data, r.value(i))
			if r.data[i] == ',' {
				i = skipSpace(r.data, i+1)
			}
		}
		r.out = append(r.out, ']')
		return i + 1
	default:
		end := scalarEnd(r.data, i)
		r.out = append(r.out, r.data[i:end]...)
		return end
	}
}

func (r *keyRewriter) object(i int) int {
	snakeKeys := r.collisions[r.objects]
	r.objects++

	r.out = append(r.out, '{')
	i = skipSpace(r.data, i+1)
	first := true
	for r.data[i] != '}' {
		end := stringEnd(r.data, i)
		quotedKey := r.data[i:end]
		i = skipSpace(r.data, end) // at ':'
		i = skipSpace(r.data, i+1) // at the value

		key := keyText(quotedKey)
		camelCase := hasUpperASCII(key)
		if camelCase && snakeKeys != nil && snakeKeys[string(appendSnakeCase(nil, key))] {
			// The field is also spelled snake_case in this object and that spelling wins. The value is
			// still walked, so that objects nested in it keep their numbers.
			mark := len(r.out)
			i = r.value(i)
			r.out = r.out[:mark]
		} else {
			if !first {
				r.out = append(r.out, ',')
			}
			first = false
			switch {
			case !camelCase:
				r.out = append(r.out, quotedKey...)
			case bytes.IndexByte(quotedKey, '\\') < 0:
				// The common case: the key is written as it was, with only ASCII capitals changed.
				r.out = append(r.out, '"')
				r.out = appendSnakeCase(r.out, key)
				r.out = append(r.out, '"')
			default:
				r.out = appendJSONString(r.out, string(appendSnakeCase(nil, key)))
			}
			r.out = append(r.out, ':')
			i = r.value(i)
		}

		i = skipSpace(r.data, i)
		if r.data[i] == ',' {
			i = skipSpace(r.data, i+1)
		}
	}
	r.out = append(r.out, '}')
	return i + 1
}

// The helpers below assume a document that json.Valid accepted.

func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// stringEnd returns the index just after the string whose opening quote is at i.
func stringEnd(data []byte, i int) int {
	for i++; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// scalarEnd returns the index just after the string, number, true, false or null starting at i.
func scalarEnd(data []byte, i int) int {
	if data[i] == '"' {
		return stringEnd(data, i)
	}
	for i < len(data) {
		switch data[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// keyText returns the text of a quoted object key. An unescaped key is returned as a slice of the document;
// only a key containing escapes is decoded into a new buffer.
func keyText(quoted []byte) []byte {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return quoted[1 : len(quoted)-1]
	}
	var key string
	if err := json.Unmarshal(quoted, &key); err != nil {
		// Unreachable for a document json.Valid accepted.
		return quoted[1 : len(quoted)-1]
	}
	return []byte(key)
}

func appendJSONString(out []byte, s string) []byte {
	encoded, err := json.Marshal(s)
	if err != nil {
		// Unreachable: every Go string can be encoded.
		return append(out, `""`...)
	}
	return append(out, encoded...)
}

func hasUpperASCII(s []byte) bool {
	for _, character := range s {
		if character >= 'A' && character <= 'Z' {
			return true
		}
	}
	return false
}

// appendSnakeCase appends the snake_case spelling of a key: every ASCII capital becomes "_" and its lower case
// letter. A leading capital is not a word boundary: "Foo" is "foo", but "fooBar" is "foo_bar".
func appendSnakeCase(out []byte, key []byte) []byte {
	for i, character := range key {
		if character < 'A' || character > 'Z' {
			out = append(out, character)
			continue
		}
		if i > 0 {
			out = append(out, '_')
		}
		out = append(out, character-'A'+'a')
	}
	return out
}

// wordBoundaries counts the capitals after the first character, each of which becomes "_" plus a letter.
func wordBoundaries(key []byte) int {
	count := 0
	for i := 1; i < len(key); i++ {
		if key[i] >= 'A' && key[i] <= 'Z' {
			count++
		}
	}
	return count
}

// snakeCaseEquals reports whether the snake_case spelling of camel is snake, without building it.
func snakeCaseEquals(camel, snake []byte) bool {
	j := 0
	for i, character := range camel {
		if character >= 'A' && character <= 'Z' {
			if i > 0 {
				if j >= len(snake) || snake[j] != '_' {
					return false
				}
				j++
			}
			character = character - 'A' + 'a'
		}
		if j >= len(snake) || snake[j] != character {
			return false
		}
		j++
	}
	return j == len(snake)
}
