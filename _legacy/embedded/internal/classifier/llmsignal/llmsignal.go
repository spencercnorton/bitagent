// Package llmsignal carries the LLM matcher's English-track read out of a
// classifier run. The extraction happens deep inside find_match, which
// DISCARDS the action's result on ErrUnmatched — but an unmatched anime
// extraction still carries the English signal worth persisting (a rejected
// raw is exactly the 'none' case, and 'none' is assignable ONLY here: the
// deterministic "\braw\b" regex collides with fansub group names). Mirrors
// the evaltrace pattern: a holder attached to the context by the processor
// before the run; with no holder every call is a no-op, so the eval and
// batch-warm paths pay nothing.
package llmsignal

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/model"
)

type ctxKey struct{}

// holder is written from the same goroutine that installed it (the processor
// runs one classification per goroutine and the matcher runs synchronously
// inside it), so no lock is needed.
type holder struct {
	english model.EnglishAudio
	valid   bool
}

// WithHolder attaches a fresh holder for one classifier run.
func WithHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, &holder{})
}

// Record stores a definite English-track read (dub|sub|none) for an anime
// extraction. Non-anime reads and "unknown" are dropped: english_audio is an
// anime-only column and unknown carries no signal. No-op without a holder.
func Record(ctx context.Context, english string, isAnime bool) {
	h, _ := ctx.Value(ctxKey{}).(*holder)
	if h == nil || !isAnime {
		return
	}
	switch model.EnglishAudio(english) {
	case model.EnglishAudioDub, model.EnglishAudioSub, model.EnglishAudioNone:
		h.english = model.EnglishAudio(english)
		h.valid = true
	}
}

// English returns the recorded value; invalid when nothing definite was
// recorded or no holder is attached.
func English(ctx context.Context) model.NullEnglishAudio {
	h, _ := ctx.Value(ctxKey{}).(*holder)
	if h == nil || !h.valid {
		return model.NullEnglishAudio{}
	}
	return model.NewNullEnglishAudio(h.english)
}
