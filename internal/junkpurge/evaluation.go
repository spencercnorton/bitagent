package junkpurge

import "encoding/json"

// EvaluationPrompt returns the exact conservative policy used by the
// production junk judge.
func EvaluationPrompt() string {
	return judgeInstructions
}

// EvaluationChatRequestJSON serializes the exact synchronous hosted request
// used by the production junk judge, including its single-newline /no_think
// delimiter and explicit stream:false field.
func EvaluationChatRequestJSON(model, name string) ([]byte, error) {
	return json.Marshal(buildChatRequest(model, name))
}

// EvaluationParseJudgment applies the exact production judgment decoder.
func EvaluationParseJudgment(text string) (Judgment, error) {
	return parseJudgment(text)
}
