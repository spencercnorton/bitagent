package llmmatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Model replies are not a wire format we control. Both stages used to hand the
// raw message content straight to json.Unmarshal into structs with plain int
// fields, which meant three routine model behaviours were hard decode errors:
//
//   - a ```json fence around the object
//   - a <think>…</think> preamble from a reasoning model
//   - "season": null for an absent integer
//
// gpt-5.4-nano happens to do none of those, so the strictness went unnoticed
// while it was the only model on this path. Measured against the deployed
// request body (n=20), it disqualified every open model tried:
// granite-4.1-8b 16/20 (null ints), nova-micro 10/20 (fences + null ints),
// gemma-3-12b 0/20. The content filter never had this problem because its
// parser already strips fences and reasoning blocks.
//
// The leniency lives here rather than on Extraction.UnmarshalJSON on purpose:
// it applies to MODEL REPLIES only, not to our own capture/eval serialisation
// of the same structs, which is well-formed by construction and should keep
// failing loudly if it ever is not.

// sanitizeModelJSON extracts the JSON object from a model reply that may carry
// a reasoning preamble, a markdown fence, or prose on either side. It returns
// the input unchanged when it cannot find a better candidate, so a genuinely
// malformed reply still surfaces as a decode error rather than silently
// becoming an empty object.
func sanitizeModelJSON(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))

	// Reasoning models emit their trace first; keep only what follows.
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = strings.TrimSpace(s[i+len("</think>"):])
	}

	// Strip one fenced block: ```json\n{...}\n``` or ```\n{...}\n```.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			// Drop the info string ("json") on the opening fence line.
			if tag := strings.TrimSpace(s[:i]); !strings.Contains(tag, "{") {
				s = s[i+1:]
			}
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}

	// Anything else (leading prose, trailing commentary): take the first
	// balanced object.
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		if obj := firstBalancedObject(s); obj != "" {
			s = obj
		}
	}
	if s == "" {
		return raw
	}
	return []byte(s)
}

// firstBalancedObject returns the first brace-balanced JSON object in s,
// ignoring braces inside string literals. Empty when there is none.
func firstBalancedObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// brace inside a string literal is not structural
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// coerceInt accepts a JSON number, a numeric string, or null, and yields 0 for
// null / empty / unparseable. A model writing "season": null or "year": "2026"
// is answering the question; it should not fail the request.
func coerceInt(raw json.RawMessage) int {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "null") {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, strconv.IntSize); err == nil {
		return int(n)
	}
	// "2026.0" and friends.
	if f, err := strconv.ParseFloat(s, 64); err == nil &&
		!math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f {
		// Format the integral float back to base 10 and let ParseInt enforce
		// the platform int range before conversion. Converting an out-of-range
		// float directly to int is implementation-dependent.
		integral := strconv.FormatFloat(f, 'f', -1, 64)
		if n, parseErr := strconv.ParseInt(
			integral,
			10,
			strconv.IntSize,
		); parseErr == nil {
			return int(n)
		}
	}
	return 0
}

// coerceFloat mirrors coerceInt for confidence scores.
func coerceFloat(raw json.RawMessage) float64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	s = strings.Trim(s, `"`)
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil &&
		!math.IsNaN(f) && !math.IsInf(f, 0) {
		return f
	}
	return 0
}

// coerceBool accepts true/false, "true"/"false", and 0/1 while preserving
// whether the model actually supplied a valid value. Missing, null, and an
// unrecognised scalar must not silently become the action-safe-looking false.
func coerceBool(raw json.RawMessage) (bool, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || strings.EqualFold(s, "null") {
		return false, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		s = strings.TrimSpace(text)
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b, true
	}
	switch s {
	case "0":
		return false, true
	case "1":
		return true, true
	default:
		return false, false
	}
}

// coerceString accepts a JSON string or null; a non-string (a number, an
// object) yields "" rather than an error, which normalizeExtraction then reads
// as "the model declined" via Extraction.OK.
func coerceString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var out string
	if err := json.Unmarshal(raw, &out); err == nil {
		return out
	}
	return ""
}

// requireJSONObject sanitizes a model reply and insists the result is a JSON
// OBJECT. This is load-bearing: encoding/json accepts a bare `null` into a
// struct with no error and leaves it zero-valued, so without this a stream of
// `null` replies from a broken endpoint would decode as "every torrent
// declined" — indistinguishable from healthy operation, and silent. Same for a
// bare number, string or array.
func requireJSONObject(raw []byte) ([]byte, error) {
	clean := bytes.TrimSpace(sanitizeModelJSON(raw))
	if len(clean) == 0 || clean[0] != '{' {
		return nil, fmt.Errorf(
			"model reply is not a JSON object: %.80q", string(clean),
		)
	}
	return clean, nil
}

// rawExtraction is the tolerant wire shape for a stage-1 reply. Every field is
// RawMessage so a wrong scalar type is a per-field problem, not a whole-reply
// failure.
type rawExtraction struct {
	Title   json.RawMessage `json:"title"`
	Year    json.RawMessage `json:"year"`
	Type    json.RawMessage `json:"type"`
	Season  json.RawMessage `json:"season"`
	Episode json.RawMessage `json:"episode"`
	IsAnime json.RawMessage `json:"is_anime"`
	IsPack  json.RawMessage `json:"is_pack"`
	IsAdult json.RawMessage `json:"is_adult"`
	English json.RawMessage `json:"english"`
}

// decodeExtraction parses a stage-1 model reply. A reply without a usable title
// is a clean model decline. Once a title is proposed, however, every
// action-critical field must be present and valid: missing is_pack/is_adult
// cannot be treated as false, and missing is_anime cannot bypass the anime
// English gate. Integer metadata remains tolerant of null/numeric strings.
func decodeExtraction(raw []byte) (Extraction, error) {
	clean, err := requireJSONObject(raw)
	if err != nil {
		return Extraction{}, err
	}
	var re rawExtraction
	if err := json.Unmarshal(clean, &re); err != nil {
		return Extraction{}, fmt.Errorf("not a JSON object: %w", err)
	}
	title := coerceString(re.Title)
	if strings.TrimSpace(title) == "" {
		return Extraction{}, nil
	}
	mediaType := strings.ToLower(strings.TrimSpace(coerceString(re.Type)))
	if mediaType != "movie" && mediaType != "tv" {
		return Extraction{}, fmt.Errorf("missing or invalid type")
	}
	isAnime, animeOK := coerceBool(re.IsAnime)
	isPack, packOK := coerceBool(re.IsPack)
	isAdult, adultOK := coerceBool(re.IsAdult)
	if !animeOK || !packOK || !adultOK {
		return Extraction{}, fmt.Errorf(
			"missing or invalid is_anime/is_pack/is_adult",
		)
	}
	english := strings.ToLower(strings.TrimSpace(coerceString(re.English)))
	if isAnime {
		switch english {
		case EnglishDub, EnglishSub, EnglishNone, EnglishUnknown:
		default:
			return Extraction{}, fmt.Errorf(
				"missing or invalid english for anime extraction",
			)
		}
	}
	return Extraction{
		Title:   title,
		Year:    coerceInt(re.Year),
		Type:    mediaType,
		Season:  coerceInt(re.Season),
		Episode: coerceInt(re.Episode),
		IsAnime: isAnime,
		IsPack:  isPack,
		IsAdult: isAdult,
		English: english,
	}, nil
}

// decodeRerank parses a stage-2 model reply. Same tolerance; the caller still
// enforces that the chosen id was one actually offered.
func decodeRerank(raw []byte) (int64, float64, error) {
	clean, err := requireJSONObject(raw)
	if err != nil {
		return 0, 0, err
	}
	var re struct {
		TmdbID     json.RawMessage `json:"tmdb_id"`
		Confidence json.RawMessage `json:"confidence"`
	}
	if err := json.Unmarshal(clean, &re); err != nil {
		return 0, 0, fmt.Errorf("not a JSON object: %w", err)
	}
	return int64(coerceInt(re.TmdbID)), coerceFloat(re.Confidence), nil
}
