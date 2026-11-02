// Package schema validates job input against capability JSON Schemas.
package schema

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// denyLoader stops schemas from pulling in local files or URLs through $ref.
// Schemas come from node manifests, so a node must not be able to make the
// gateway read arbitrary paths.
type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are not allowed: %s", url)
}

// Compile parses a JSON Schema document.
func Compile(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(denyLoader{})
	if err := compiler.AddResource("mem://input.json", doc); err != nil {
		return nil, fmt.Errorf("add schema: %w", err)
	}
	compiled, err := compiler.Compile("mem://input.json")
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return compiled, nil
}

// Validate checks a JSON document against a compiled schema.
func Validate(compiled *jsonschema.Schema, input []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return fmt.Errorf("input is not valid JSON: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(ve.Error())
		}
		return err
	}
	return nil
}
