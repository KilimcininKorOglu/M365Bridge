package toolcalling

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/KilimcininKorOglu/M365Bridge/pkg/logging"
)

// SchemaByTool maps each declared tool name to its JSON schema. It supports the
// three provider shapes a ToolDef can carry: Anthropic `input_schema`,
// Responses top-level `parameters`, and OpenAI Chat Completions
// `function.parameters`. Tools without a schema map to nil.
func SchemaByTool(tools []ToolDef) map[string]map[string]any {
	out := make(map[string]map[string]any, len(tools))
	for i := range tools {
		name := ToolName(&tools[i])
		if name == "" {
			continue
		}
		for _, schema := range []map[string]any{
			tools[i].InputSchema,
			tools[i].Parameters,
			tools[i].Function.Parameters,
		} {
			if len(schema) > 0 {
				out[name] = schema
				break
			}
		}
		if _, ok := out[name]; !ok {
			out[name] = nil
		}
	}
	return out
}

// ToolContracts carries everything the parser needs to validate a tool call:
// the required argument names and the full JSON schema, both keyed by tool
// name. It travels as one value so every parse path enforces the same contract.
type ToolContracts struct {
	Required map[string][]string
	Schemas  map[string]map[string]any
	// Choice is the request's tool_choice: "auto", "none", "required", or the
	// name of the one tool the caller pinned. The prompt asks the model to obey
	// it, but a model that ignores the instruction must not reach the client,
	// so it is enforced here as well.
	Choice string
	// NoParallel carries a caller that refused parallel tool calls, through
	// OpenAI's parallel_tool_calls or Anthropic's disable_parallel_tool_use.
	// Such a caller executes one call per turn, so a second call in the same
	// response would either be dropped by the client or answered out of order.
	NoParallel bool
}

// ContractsFor builds the validation contracts for the declared tools.
func ContractsFor(tools []ToolDef) ToolContracts {
	return ToolContracts{
		Required: RequiredArgsByTool(tools),
		Schemas:  SchemaByTool(tools),
	}
}

// WithChoice returns the contracts with the request's tool_choice attached.
func (c ToolContracts) WithChoice(choice string) ToolContracts {
	c.Choice = choice
	return c
}

// WithoutParallel returns the contracts with parallel tool calls refused when
// disabled is true. An absent request field means parallel calls are allowed,
// which is the default in both provider protocols.
func (c ToolContracts) WithoutParallel(disabled bool) ToolContracts {
	c.NoParallel = disabled
	return c
}

// keepFirstWhenSerial returns the calls the caller is willing to receive. A
// caller that refused parallel calls gets the first one; the rest are dropped
// rather than reordered, because the model emits them in the order it wants
// them run and a later round can ask for the next one.
func (c ToolContracts) keepFirstWhenSerial(calls []ToolCall) []ToolCall {
	if !c.NoParallel || len(calls) <= 1 {
		return calls
	}
	for _, dropped := range calls[1:] {
		logging.Warnf("tool calls: dropping %q, the request refused parallel tool calls", dropped.Name)
	}
	return calls[:1]
}

// choiceAllows reports whether tool_choice permits calling name.
func (c ToolContracts) choiceAllows(name string) bool {
	switch c.Choice {
	case "", "auto", "required", "any":
		return true
	case "none":
		return false
	}
	// Anything else names the single tool the caller pinned.
	return c.Choice == name
}

// validate checks one tool call's arguments. It returns the arguments to
// forward, which may have had undeclared keys pruned, a rejection reason that
// is empty when the call is acceptable, and whether re-asking the model could
// plausibly fix the rejection.
func (c ToolContracts) validate(name string, arguments json.RawMessage) (result json.RawMessage, reason string, repairable bool) {
	if !c.choiceAllows(name) {
		if c.Choice == "none" {
			// The caller asked for no tool calls at all, so re-asking would
			// only burn a turn.
			return arguments, `tool_choice is "none", so no tool may be called`, false
		}
		return arguments, fmt.Sprintf("tool_choice pins %q, so %q may not be called", c.Choice, name), true
	}
	if !toolCallSatisfiesRequired(arguments, c.Required[name]) {
		return arguments, "required arguments were missing or empty", true
	}

	schema := c.Schemas[name]
	if len(schema) == 0 {
		return arguments, "", false
	}

	var decoded map[string]any
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		// Only an object can be validated. A non-object payload already failed
		// the required check unless the tool declares no required arguments.
		return arguments, "arguments were not a JSON object", true
	}
	if err := ValidateAndPrune(decoded, schema); err != nil {
		return arguments, err.Error(), true
	}

	pruned, err := json.Marshal(decoded)
	if err != nil {
		return arguments, "", false
	}
	return pruned, "", false
}

// ValidateAndPrune checks arguments against a JSON schema and removes
// properties the schema does not declare when it sets
// `additionalProperties: false`.
//
// Pruning rather than rejecting keeps a call the model got right in substance
// but decorated with an extra field, which would otherwise be dropped and cost
// a whole round trip. Everything else is a genuine contract violation: a wrong
// type or an out-of-enum value would make the client's tool misbehave, so the
// call is reported invalid instead of being forwarded.
//
// A nil schema accepts any arguments, because a tool declared without one
// states no contract to enforce.
func ValidateAndPrune(args map[string]any, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	return validateNode(args, schema, "arguments")
}

// validateNode walks one schema node. Container nodes recurse into their
// children so nested objects and array items are checked too.
func validateNode(value any, schema map[string]any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if err := validateEnum(value, schema, path); err != nil {
		return err
	}

	declared := schemaType(schema)
	switch declared {
	case "object":
		return validateObjectNode(value, schema, path)
	case "array":
		return validateArrayNode(value, schema, path)
	}
	return validateScalar(value, declared, path)
}

// validateObjectNode checks that the value is an object before walking it.
func validateObjectNode(value any, schema map[string]any, path string) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be an object", path)
	}
	return validateObject(object, schema, path)
}

// validateArrayNode checks that the value is an array before walking its items
// against the item schema.
func validateArrayNode(value any, schema map[string]any, path string) error {
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", path)
	}
	itemSchema, _ := schema["items"].(map[string]any)
	for i, item := range items {
		if err := validateNode(item, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

// isJSONInteger reports whether a number carries no fractional part.
func isJSONInteger(value any) bool {
	number, ok := jsonNumber(value)
	return ok && math.Trunc(number) == number
}

// scalarCheckers maps a scalar schema type to the check that accepts it. A type
// with no entry, which includes an absent type and a union type, accepts
// anything.
var scalarCheckers = map[string]func(any) bool{
	"string":  func(value any) bool { _, ok := value.(string); return ok },
	"boolean": func(value any) bool { _, ok := value.(bool); return ok },
	"number":  func(value any) bool { _, ok := jsonNumber(value); return ok },
	"integer": isJSONInteger,
	"null":    func(value any) bool { return value == nil },
}

// scalarTypeNames names each scalar type the way its refusal message reads.
var scalarTypeNames = map[string]string{
	"string":  "a string",
	"boolean": "a boolean",
	"number":  "a number",
	"integer": "an integer",
	"null":    "null",
}

// validateScalar checks a value against a scalar schema type.
func validateScalar(value any, declared, path string) error {
	accepts, enforced := scalarCheckers[declared]
	if !enforced || accepts(value) {
		return nil
	}
	return fmt.Errorf("%s must be %s", path, scalarTypeNames[declared])
}

// validateObject enforces required keys, prunes undeclared keys when the schema
// forbids them, and recurses into declared properties.
func validateObject(object map[string]any, schema map[string]any, path string) error {
	if err := requireDeclaredKeys(object, schema, path); err != nil {
		return err
	}

	properties, _ := schema["properties"].(map[string]any)
	pruneUndeclaredKeys(object, properties, schema)

	for name, child := range object {
		childSchema, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		if err := validateNode(child, childSchema, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

// requireDeclaredKeys refuses an object that omits a required argument.
func requireDeclaredKeys(object map[string]any, schema map[string]any, path string) error {
	for _, name := range requiredFromSchema(schema) {
		if _, ok := object[name]; !ok {
			return fmt.Errorf("%s is missing required argument %q", path, name)
		}
	}
	return nil
}

// pruneUndeclaredKeys drops the keys a closed schema does not declare, because
// a model that invented an extra argument is corrected rather than refused.
func pruneUndeclaredKeys(object map[string]any, properties map[string]any, schema map[string]any) {
	allowed, declaredClosed := schema["additionalProperties"].(bool)
	if !declaredClosed || allowed || len(properties) == 0 {
		return
	}
	for name := range object {
		if _, declared := properties[name]; !declared {
			delete(object, name)
		}
	}
}

// validateEnum rejects a value the schema does not list. Comparison goes
// through JSON so numbers and strings match the way the caller wrote them.
func validateEnum(value any, schema map[string]any, path string) error {
	allowed, ok := schema["enum"].([]any)
	if !ok || len(allowed) == 0 {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s is not an allowed value", path)
	}
	for _, candidate := range allowed {
		if other, err := json.Marshal(candidate); err == nil && string(other) == string(encoded) {
			return nil
		}
	}
	return fmt.Errorf("%s is not an allowed value", path)
}

// schemaType reads the schema's declared type. A union type is treated as
// unconstrained, because enforcing one branch would reject valid input.
func schemaType(schema map[string]any) string {
	declared, _ := schema["type"].(string)
	return declared
}

// jsonNumber accepts the numeric shapes a decoded JSON document or a
// programmatic definition can produce.
func jsonNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	}
	return 0, false
}
