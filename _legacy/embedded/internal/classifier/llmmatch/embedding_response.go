package llmmatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode"
)

// Vectors are returned in input-index order, then normalized for a stable dot
// product. No similarity value is converted into an attachment probability.
func decodeEmbeddingVectors(raw []byte, expected int, cfg EmbeddingConfig) ([][]float64, error) {
	if err := uniqueEmbeddingJSON(raw); err != nil {
		return nil, err
	}
	var response struct {
		Data   []json.RawMessage `json:"data"`
		Error  json.RawMessage   `json:"error"`
		Status string            `json:"status"`
		Model  string            `json:"model"`
		Object string            `json:"object"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Data) != expected || expected < 2 {
		return nil, errors.New("embedding response count mismatch")
	}
	if (len(response.Error) != 0 && string(bytes.TrimSpace(response.Error)) != "null") ||
		(response.Status != "" && response.Status != "completed") {
		return nil, errors.New("embedding provider returned an error or incomplete response")
	}
	if response.Model != "" && response.Model != cfg.Model {
		return nil, errors.New("embedding response model does not match the requested model")
	}
	if response.Object != "" && response.Object != "list" {
		return nil, errors.New("embedding response has the wrong object type")
	}
	ordered := make([][]float64, expected)
	dimensions := 0
	for _, rawRow := range response.Data {
		var fields map[string]json.RawMessage
		if json.Unmarshal(rawRow, &fields) != nil || fields == nil {
			return nil, errors.New("embedding row must be an object")
		}
		for field := range fields {
			if field != "index" && field != "embedding" && field != "object" {
				return nil, errors.New("unknown embedding row field")
			}
		}
		var row struct {
			Index     *int       `json:"index"`
			Embedding []*float64 `json:"embedding"`
			Object    string     `json:"object"`
		}
		if json.Unmarshal(rawRow, &row) != nil || row.Index == nil || *row.Index < 0 || *row.Index >= expected ||
			ordered[*row.Index] != nil || len(row.Embedding) == 0 || len(row.Embedding) > cfg.MaxDimensions {
			return nil, errors.New("invalid embedding index or dimensions")
		}
		if row.Object != "" && row.Object != "embedding" {
			return nil, errors.New("embedding row has the wrong object type")
		}
		if dimensions == 0 {
			dimensions = len(row.Embedding)
		}
		if len(row.Embedding) != dimensions || (cfg.Dimensions > 0 && dimensions != cfg.Dimensions) {
			return nil, errors.New("embedding dimension mismatch")
		}
		vector := make([]float64, dimensions)
		var scale float64
		for index, component := range row.Embedding {
			if component == nil || math.IsNaN(*component) || math.IsInf(*component, 0) {
				return nil, errors.New("nonfinite or null embedding component")
			}
			vector[index] = *component
			scale = math.Max(scale, math.Abs(*component))
		}
		if scale == 0 {
			return nil, errors.New("zero embedding")
		}
		// Scaling first prevents finite large/small components from overflowing
		// or underflowing the norm before the cosine calculation.
		var norm float64
		for index := range vector {
			vector[index] /= scale
			norm += vector[index] * vector[index]
		}
		norm = math.Sqrt(norm)
		for index := range vector {
			vector[index] /= norm
		}
		ordered[*row.Index] = vector
	}
	return ordered, nil
}

func uniqueEmbeddingJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var read func(int) error
	read = func(depth int) error {
		if depth > 64 {
			return errors.New("embedding JSON nesting exceeds limit")
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
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[embeddingFieldFold(key)] {
					return errors.New("duplicate embedding JSON field")
				}
				seen[embeddingFieldFold(key)] = true
				if err := read(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := read(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected embedding JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := read(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing embedding JSON data")
	}
	return nil
}

func embeddingFieldFold(name string) string {
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
