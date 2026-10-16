package llmstage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// encoding/json otherwise accepts duplicate object fields by choosing the
// last value, including case variants of struct fields. Reject ambiguity in
// both the provider envelope and its model-authored answer before decoding.
// Provider metadata is extensible; the answer's field allowlist is separate.
func validateUniqueResponseJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("response JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[responseFieldFold(name)] {
					return errors.New("duplicate response JSON field")
				}
				seen[responseFieldFold(name)] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected response JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing response JSON data")
	}
	return nil
}

// Match encoding/json's Unicode simple-fold equivalence, including aliases
// such as long s. Lowercasing alone does not cover that equivalence class.
func responseFieldFold(name string) string {
	return strings.Map(func(character rune) rune {
		minimum := character
		for next := unicode.SimpleFold(character); next != character; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		return minimum
	}, name)
}
