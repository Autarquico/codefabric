package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MapJSONLines applies fn to every line of raw, which must be one JSON object
// each, and ends every line of the result with a newline.
func MapJSONLines(raw []byte, fn func(line []byte) ([]byte, error)) ([]byte, error) {
	var out bytes.Buffer
	for n := 1; len(raw) > 0; n++ {
		var line []byte
		line, raw, _ = bytes.Cut(raw, []byte("\n"))
		line, err := fn(line)
		if err != nil {
			return nil, fmt.Errorf("line %d is not a JSON object: %w", n, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// SetJSONStrings sets the string values at the dotted paths of edits ("cwd",
// "payload.cwd") in a line that is one JSON object, leaving every other byte
// as it was. A path that is not there, or whose value is not a string, is
// left alone.
func SetJSONStrings(line []byte, edits map[string]string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	var spans []editSpan
	if err := scanObject(dec, "", edits, &spans); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the object")
	}
	if len(spans) == 0 {
		return line, nil
	}
	var out bytes.Buffer
	pos := 0
	for _, e := range spans {
		out.Write(line[pos:e.start])
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(e.value); err != nil {
			return nil, err
		}
		out.Truncate(out.Len() - 1) // the newline of Encode
		pos = e.end
	}
	out.Write(line[pos:])
	return out.Bytes(), nil
}

type editSpan struct {
	start, end int
	value      string
}

// scanObject reads the members of the object whose '{' dec has read, up to
// and including its '}', and records the spans of the values to edit.
func scanObject(dec *json.Decoder, prefix string, edits map[string]string, spans *[]editSpan) error {
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		path := prefix + key
		if want, ok := edits[path]; ok {
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				return err
			}
			var current string
			if json.Unmarshal(value, &current) == nil {
				end := int(dec.InputOffset())
				*spans = append(*spans, editSpan{start: end - len(value), end: end, value: want})
			}
			continue
		}
		if !hasEditUnder(edits, path+".") {
			if err := dec.Decode(&json.RawMessage{}); err != nil {
				return err
			}
			continue
		}
		open, err := dec.Token()
		if err != nil {
			return err
		}
		switch open {
		case json.Delim('{'):
			if err := scanObject(dec, path+".", edits, spans); err != nil {
				return err
			}
		case json.Delim('['):
			for depth := 1; depth > 0; {
				t, err := dec.Token()
				if err != nil {
					return err
				}
				switch t {
				case json.Delim('['), json.Delim('{'):
					depth++
				case json.Delim(']'), json.Delim('}'):
					depth--
				}
			}
		}
	}
	_, err := dec.Token() // the '}'
	return err
}

func hasEditUnder(edits map[string]string, prefix string) bool {
	for path := range edits {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
