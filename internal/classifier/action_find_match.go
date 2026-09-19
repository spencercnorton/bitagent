package classifier

import (
	"errors"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/evaltrace"
)

const findMatchName = "find_match"

type findMatchAction struct{}

func (findMatchAction) name() string {
	return findMatchName
}

var findMatchActionPayloadSpec = payloadSingleKeyValue[[]any]{
	key: findMatchName,
	valueSpec: payloadMustSucceed[[]any]{payloadList[any]{itemSpec: payloadGeneric[any]{
		jsonSchema: map[string]any{
			"$ref": "#/definitions/action_single",
		},
	}}},
	description: "Iterate through a series of actions to find the first that does not return an unmatched error",
}

func (findMatchAction) compileAction(ctx compilerContext) (action, error) {
	payload, err := findMatchActionPayloadSpec.Unmarshal(ctx)
	if err != nil {
		return action{}, ctx.error(err)
	}

	actions := make([]action, len(payload))

	for i, actionPayload := range payload {
		a, err := ctx.compileAction(ctx.child(numericPathPart(i), actionPayload))
		if err != nil {
			return action{}, err
		}

		actions[i] = a
	}

	path := ctx.path

	return action{
		run: func(ctx executionContext) (classification.Result, error) {
			for _, action := range actions {
				result, err := action.run(ctx)
				if err != nil {
					if errors.Is(err, classification.ErrUnmatched) {
						continue
					}
					return classification.Result{}, classification.RuntimeError{
						Cause: err,
						Path:  path,
					}
				}
				evaltrace.Record(ctx, action.name)
				return result, nil
			}
			return ctx.result, nil
		},
	}, nil
}

func (findMatchAction) JSONSchema() JSONSchema {
	return findMatchActionPayloadSpec.JSONSchema()
}
