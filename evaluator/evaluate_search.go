package evaluator

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strings"

	"github.com/PaesslerAG/jsonpath"
	"github.com/bradleyjkemp/sigma-go"
)

func (rule RuleEvaluator) evaluateSearchExpression(search sigma.SearchExpr, event Event) bool {
	switch s := search.(type) {
	case sigma.And:
		for _, node := range s {
			if !rule.evaluateSearchExpression(node, event) {
				return false
			}
		}
		return true

	case sigma.Or:
		for _, node := range s {
			if rule.evaluateSearchExpression(node, event) {
				return true
			}
		}
		return false

	case sigma.Not:
		return !rule.evaluateSearchExpression(s.Expr, event)

	case sigma.SearchIdentifier:
		search, ok := rule.Detection.Searches[s.Name]
		if !ok {
			panic("invalid search identifier")
		}
		return rule.evaluateSearch(search, event)

	case sigma.OneOfThem:
		for name := range rule.Detection.Searches {
			if rule.evaluateSearchExpression(sigma.SearchIdentifier{Name: name}, event) {
				return true
			}
		}
		return false

	case sigma.OneOfPattern:
		for name := range rule.Detection.Searches {
			matchesPattern, err := path.Match(s.Pattern, name)
			if err != nil {
				panic(err)
			}
			if !matchesPattern {
				continue
			}
			if rule.evaluateSearchExpression(sigma.SearchIdentifier{Name: name}, event) {
				return true
			}
		}
		return false

	case sigma.AllOfThem:
		for name := range rule.Detection.Searches {
			if !rule.evaluateSearchExpression(sigma.SearchIdentifier{Name: name}, event) {
				return false
			}
		}
		return true

	case sigma.AllOfPattern:
		for name := range rule.Detection.Searches {
			matchesPattern, err := path.Match(s.Pattern, name)
			if err != nil {
				panic(err)
			}
			if !matchesPattern {
				continue
			}
			if !rule.evaluateSearchExpression(sigma.SearchIdentifier{Name: name}, event) {
				return false
			}
		}
		return true
	}

	panic(false)
}

func (rule RuleEvaluator) evaluateSearch(search sigma.Search, event Event) bool {
	if len(search.Keywords) > 0 {
		panic("keywords unsupported")
	}

	// A Search is a series of "does this field match this value" conditions
	// all need to match, for the Search to evaluate to true
	for _, matcher := range search.FieldMatchers {
		// A field matcher can specify multiple values to match against
		// either the field should match all of these values or it should match any of them
		allValuesMustMatch := false
		fieldModifiers := matcher.Modifiers
		if len(matcher.Modifiers) > 0 && fieldModifiers[len(fieldModifiers)-1] == "all" {
			allValuesMustMatch = true
			fieldModifiers = fieldModifiers[:len(fieldModifiers)-1]
		}

		// field matchers can specify modifiers (FieldName|modifier1|modifier2) which change the matching behaviour
		comparator := baseComparator
		for _, name := range fieldModifiers {
			if modifiers[name] == nil {
				panic(fmt.Errorf("unsupported modifier %s", name))
			}
			comparator = modifiers[name](comparator)
		}

		if !rule.matcherMatchesValues(matcher, comparator, allValuesMustMatch, event) {
			// this field didn't match so the overall matcher doesn't match
			return false
		}
	}

	// all fields matched
	return true
}

func (rule *RuleEvaluator) matcherMatchesValues(matcher sigma.FieldMatcher, comparator valueComparator, allValuesMustMatch bool, event Event) bool {
	// First collect this list of event values we're matching against
	var actualValues []interface{}
	if len(rule.fieldmappings[matcher.Field]) == 0 {
		// No FieldMapping exists so use the name directly from the rule
		actualValues = []interface{}{eventValue(event, matcher.Field)}
	} else {
		// FieldMapping does exist so check each of the possible mapped names instead of the name from the rule
		for _, mapping := range rule.fieldmappings[matcher.Field] {
			var v interface{}

			switch {
			case strings.HasPrefix(mapping, "$.") || strings.HasPrefix(mapping, "$["):
				v = evaluateJSONPath(mapping, event)
			default:
				v = eventValue(event, mapping)
			}

			actualValues = append(actualValues, toGenericSlice(v)...)
		}
	}

	matched := allValuesMustMatch
	for _, expectedValue := range matcher.Values {
		valueMatchedEvent := false
		// There are multiple possible event fields that each expected value needs to be compared against
		for _, actualValue := range actualValues {
			if comparator(actualValue, expectedValue) {
				valueMatchedEvent = true
				break
			}
		}

		if allValuesMustMatch {
			matched = matched && valueMatchedEvent
		} else {
			matched = matched || valueMatchedEvent
		}
	}
	return matched
}

// This is a hack because none of the JSONPath libraries expose the parsed AST :(
// Matches JSONPaths with either a $.fieldname or $["fieldname"] prefix and extracts 'fieldname'
var firstJSONPathField = regexp.MustCompile(`^\$(?:[.]|\[")([a-zA-Z0-9_\-]+)(?:"])?`)

func evaluateJSONPath(expr string, event Event) interface{} {
	// First, just try to evaluate the JSONPath expression directly
	value, err := jsonpath.Get(expr, event)
	if err == nil {
		// Got no error so return the value directly
		return value
	}
	if !strings.HasPrefix(err.Error(), "unsupported value type") {
		return nil
	}

	// Got an error: "unsupported value type X for select, expected map[string]interface{} or []interface{}"
	// This means we tried to access a nested field that hasn't yet been unmarshalled.
	// We try to fix this by finding the top-level field being selected and attempting to unmarshal it.
	// This is best effort and only works for top-level fields.
	// A longer term solution would be to either build this into the JSONPath library directly or remove this feature and let the user do it.

	jsonPathField := firstJSONPathField.FindStringSubmatch(expr)
	if jsonPathField == nil {
		panic("couldn't parse JSONPath expression")
	}

	var subValue interface{}
	switch e := event.(type) {
	case map[string]string:
		json.Unmarshal([]byte(e[jsonPathField[1]]), &subValue)
	case map[string]interface{}:
		switch sub := e[jsonPathField[1]].(type) {
		case string:
			json.Unmarshal([]byte(sub), &subValue)
		case []byte:
			json.Unmarshal(sub, &subValue)
		default:
			// Oh well, don't try to unmarshal the nested field
			value, _ := jsonpath.Get(expr, event)
			return value
		}
	}

	value, _ = jsonpath.Get(expr, map[string]interface{}{
		jsonPathField[1]: subValue,
	})
	return value
}

type valueComparator func(actual interface{}, expected string) bool

func baseComparator(actual interface{}, expected string) bool {
	return fmt.Sprintf("%v", actual) == expected
}

type valueModifier func(next valueComparator) valueComparator

var modifiers = map[string]valueModifier{
	"contains": func(next valueComparator) valueComparator {
		return func(actual interface{}, expected string) bool {
			return strings.Contains(fmt.Sprintf("%v", actual), expected)
		}
	},
	"endswith": func(next valueComparator) valueComparator {
		return func(actual interface{}, expected string) bool {
			return strings.HasSuffix(fmt.Sprintf("%v", actual), expected)
		}
	},
	"startswith": func(next valueComparator) valueComparator {
		return func(actual interface{}, expected string) bool {
			return strings.HasPrefix(fmt.Sprintf("%v", actual), expected)
		}
	},
	"base64": func(next valueComparator) valueComparator {
		return func(actual interface{}, expected string) bool {
			return next(actual, base64.StdEncoding.EncodeToString([]byte(expected)))
		}
	},
}

func toGenericSlice(v interface{}) []interface{} {
	rv := reflect.ValueOf(v)

	// if this isn't a slice, then return a slice containing the
	// original value
	if rv.Kind() != reflect.Slice {
		return []interface{}{v}
	}

	var out []interface{}
	for i := 0; i < rv.Len(); i++ {
		out = append(out, rv.Index(i).Interface())
	}

	return out
}
