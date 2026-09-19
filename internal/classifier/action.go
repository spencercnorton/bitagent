package classifier

import (
	"errors"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
)

func actions(defs ...actionDefinition) feature {
	return func(c *features) {
		c.actions = append(c.actions, defs...)
	}
}

type actionCompiler interface {
	compileAction(ctx compilerContext) (action, error)
}

type actionDefinition interface {
	HasJSONSchema
	name() string
	actionCompiler
}

func (c compilerContext) compileAction(ctx compilerContext) (action, error) {
	var rawActions []any

	isArray := false

	if s, ok := ctx.source.([]any); ok {
		rawActions = s
		isArray = true
	} else {
		rawActions = []any{ctx.source}
	}

	var actions []action

	var errs []error
outer:
	for i, rawAction := range rawActions {
		actionCtx := ctx
		if isArray {
			actionCtx = ctx.child(numericPathPart(i), rawAction)
		}
		for _, def := range c.actions {
			a, err := def.compileAction(actionCtx.child(def.name(), rawAction))
			if err == nil {
				a.name = def.name()
				actions = append(actions, a)
				continue outer
			}
			if asFatalCompilerError(err) != nil {
				return action{}, err
			}
		}
		errs = append(errs, fmt.Errorf("no action matched: %v", ctx.source))
	}

	if len(errs) > 0 {
		return action{}, errors.Join(errs...)
	}

	// A single-action source needs no sequencing wrapper. Returning the inner
	// action directly preserves its stamped name so find_match can attribute
	// the winning stage (the multi-action composite legitimately has none).
	if len(actions) == 1 {
		return actions[0], nil
	}

	return action{run: func(ctx executionContext) (classification.Result, error) {
		for _, a := range actions {
			result, err := a.run(ctx)
			if err != nil {
				return classification.Result{}, err
			}
			ctx = ctx.withResult(result)
		}
		return ctx.result, nil
	}}, nil
}

type action struct {
	run func(executionContext) (classification.Result, error)
	// name is the actionDefinition name stamped by the compile dispatcher;
	// find_match records it via evaltrace when the action attaches.
	name string
}
