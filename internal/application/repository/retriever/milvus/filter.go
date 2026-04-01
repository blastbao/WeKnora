package milvus

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"
)

const (
	// operatorAnd is the "and" operator.
	operatorAnd = "and"

	// operatorOr is the "or" operator.
	operatorOr = "or"

	// operatorEqual is the "equal" operator.
	operatorEqual = "eq"

	// operatorNotEqual is the "not equal" operator.
	operatorNotEqual = "ne"

	// operatorGreaterThan is the "greater than" operator.
	operatorGreaterThan = "gt"

	// operatorGreaterThanOrEqual is the "greater than or equal" operator.
	operatorGreaterThanOrEqual = "gte"

	// operatorLessThan is the "less than" operator.
	operatorLessThan = "lt"

	// operatorLessThanOrEqual is the "less than or equal" operator.
	operatorLessThanOrEqual = "lte"

	// operatorIn is the "in" operator.
	operatorIn = "in"

	// operatorNotIn is the "not in" operator.
	operatorNotIn = "not in"

	// operatorLike is the "contains" operator.
	operatorLike = "like"

	// operatorNotLike is the "not contains" operator.
	operatorNotLike = "not like"

	// operatorBetween is the "between" operator.
	operatorBetween = "between"
)

var comparisonOperators = map[string]string{
	operatorEqual:              "==",
	operatorNotEqual:           "!=",
	operatorGreaterThan:        ">",
	operatorGreaterThanOrEqual: ">=",
	operatorLessThan:           "<",
	operatorLessThanOrEqual:    "<=",
	operatorLike:               "like",
	operatorNotLike:            "not like",
}

// 实现了一个通用的过滤器转换器，将业务层定义的、结构化的过滤条件，
// 递归地转换为 Milvus 数据库可执行的表达式字符串以及对应的参数映射表。
//
// 这种设计通常用于支持参数化查询，既能防止 SQL/Expr 注入攻击，又能处理复杂的数据类型（如时间、特殊字符）。
//
// 举例：
//
// 业务层构造通用条件树：
//	cond := &universalFilterCondition{
//    Operator: "and",
//    Value: []*universalFilterCondition{
//        {Field: "knowledge_base_id", Operator: "eq", Value: "kb_123"},
//        {Field: "is_enabled", Operator: "eq", Value: true},
//        {Field: "tag_id", Operator: "in", Value: []string{"tag1", "tag2"}},
//    },
//	}
//
// 转换后得到：
//
// 	exprStr: "(knowledge_base_id == {knowledge_base_id_1}) and (is_enabled == {is_enabled_2}) and (tag_id in {tag_id_3})"
//	params: map[string]any{
//	   "knowledge_base_id_1": "kb_123",
//	   "is_enabled_2": true,
//	   "tag_id_3": []string{"tag1", "tag2"},
//	}

type convertResult struct {
	exprStr string
	params  map[string]any
}

type filter struct{}

func (c *filter) Convert(cond *universalFilterCondition) (*convertResult, error) {
	var counter int
	return c.convertCondition(cond, &counter)
}

func (c *filter) convertComparisonCondition(
	cond *universalFilterCondition,
	counter *int,
) (*convertResult, error) {
	condField := cond.Field
	if condField == "" || cond.Value == nil {
		return nil, fmt.Errorf("milvus filter condition is nil")
	}
	operator, ok := comparisonOperators[cond.Operator]
	if !ok {
		return nil, fmt.Errorf("unsupported comparison operator: %s", cond.Operator)
	}

	paramName := c.convertParamName(cond.Field, counter)
	return &convertResult{
		exprStr: fmt.Sprintf("%s %s {%s}", condField, operator, paramName),
		params:  map[string]any{paramName: cond.Value},
	}, nil
}

func (c *filter) convertLogicalCondition(
	cond *universalFilterCondition,
	counter *int,
) (*convertResult, error) {
	if cond.Value == nil {
		return nil, fmt.Errorf("milvus filter condition is nil")
	}
	conds, ok := cond.Value.([]*universalFilterCondition)
	if !ok {
		return nil, fmt.Errorf("invalid logical condition value type")
	}

	var condResult *convertResult
	for _, childCond := range conds {
		childRes, err := c.convertCondition(childCond, counter)
		if err != nil {
			return nil, err
		}
		if childRes == nil || childRes.exprStr == "" {
			continue
		}
		if condResult == nil {
			condResult = childRes
			continue
		}

		condResult.exprStr = fmt.Sprintf(
			"(%s) %s (%s)",
			condResult.exprStr,
			strings.ToLower(cond.Operator),
			childRes.exprStr,
		)
		maps.Copy(condResult.params, childRes.params)
	}

	if condResult == nil {
		return nil, fmt.Errorf("empty logical condition")
	}
	return condResult, nil
}

func (c *filter) convertCondition(
	cond *universalFilterCondition,
	counter *int,
) (*convertResult, error) {
	if cond == nil {
		return nil, fmt.Errorf("milvus filter condition is nil")
	}
	switch cond.Operator {
	case operatorEqual, operatorNotEqual, operatorGreaterThan,
		operatorGreaterThanOrEqual, operatorLessThan,
		operatorLessThanOrEqual, operatorLike, operatorNotLike:
		return c.convertComparisonCondition(cond, counter)
	case operatorAnd, operatorOr:
		return c.convertLogicalCondition(cond, counter)
	case operatorIn, operatorNotIn:
		return c.convertInCondition(cond, counter)
	case operatorBetween:
		return c.convertBetweenCondition(cond, counter)
	default:
		return nil, fmt.Errorf("unsupported operator: %v", cond.Operator)
	}
}

func (c *filter) convertInCondition(
	cond *universalFilterCondition,
	counter *int,
) (*convertResult, error) {
	condField := cond.Field
	if condField == "" || cond.Value == nil {
		return nil, fmt.Errorf("milvus filter condition is nil")
	}

	s := reflect.ValueOf(cond.Value)
	if s.Kind() != reflect.Slice || s.Len() <= 0 {
		return nil, fmt.Errorf("in operator value must be a slice with at least one value: %v", cond.Value)
	}

	paramName := c.convertParamName(cond.Field, counter)
	return &convertResult{
		exprStr: fmt.Sprintf("%s %s {%s}", condField, strings.ToLower(cond.Operator), paramName),
		params:  map[string]any{paramName: cond.Value},
	}, nil
}

func (c *filter) convertBetweenCondition(
	cond *universalFilterCondition,
	counter *int,
) (*convertResult, error) {
	condField := cond.Field
	if condField == "" || cond.Value == nil {
		return nil, fmt.Errorf("milvus filter condition is nil")
	}

	value := reflect.ValueOf(cond.Value)
	if value.Kind() != reflect.Slice || value.Len() != 2 {
		return nil, fmt.Errorf("between operator value must be a slice with two elements: %v", cond.Value)
	}

	paramBase := c.convertParamName(cond.Field, counter)
	paramName1 := fmt.Sprintf("%s_%d", paramBase, 0)
	paramName2 := fmt.Sprintf("%s_%d", paramBase, 1)
	return &convertResult{
		exprStr: fmt.Sprintf("%s >= {%s} and %s <= {%s}", condField, paramName1, condField, paramName2),
		params: map[string]any{
			paramName1: value.Index(0).Interface(),
			paramName2: value.Index(1).Interface(),
		},
	}, nil
}

func formatValue(value any) string {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf("\"%s\"", escapeDoubleQuotes(v))
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%d", v)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case float32, float64:
		return fmt.Sprintf("%v", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case time.Time:
		return fmt.Sprintf("%d", v.Unix())
	default:
		return fmt.Sprintf("\"%v\"", value)
	}
}

// escapeDoubleQuotes escapes double quotes in a string for use in Milvus expressions.
func escapeDoubleQuotes(s string) string {
	return strings.ReplaceAll(s, "\"", "\\\"")
}

// convertParamName converts field name to a valid Milvus template parameter name.
// Milvus template parameters don't support '.' character, so we replace it with '_'.
func (c *filter) convertParamName(field string, counter *int) string {
	*counter++
	return fmt.Sprintf("%s_%d", strings.ReplaceAll(field, ".", "_"), *counter)
}

// 结构说明
//
//	Field: 字段名（如 source_id, create_time）。
//	Operator: 操作符（eq, gt, and, or, in, between 等）。
//	Value:
//	 - 对于比较操作（eq, gt）：是具体的值（字符串、数字等）。
//	 - 对于逻辑操作（and, or）：是一个子条件切片 []*universalFilterCondition。
//	 - 对于集合操作（in）：是一个切片。
//	 - 对于范围操作（between）：是一个包含两个元素的切片 [min, max]。

type universalFilterCondition struct {
	Field    string `json:"field,omitempty" jsonschema:"description=The metadata field to filter on (required for comparison operators)"`
	Operator string `json:"operator" jsonschema:"description=The operator to use,enum=eq,enum=ne,enum=gt,enum=gte,enum=lt,enum=lte,enum=in,enum=not in,enum=like,enum=not like,enum=between,enum=and,enum=or"`
	Value    any    `json:"value,omitempty" jsonschema:"description=The value to compare against (for comparison operators) or array of sub-conditions (for logical operators and/or)"`
}

// JSON 解析
//	自动递归：
// 		当检测到操作符是 and 或 or 时，它会自动将 JSON 中的 value 数组反序列化为 []*universalFilterCondition。
//		这使得前端或调用方可以传入无限嵌套的 JSON 结构，而无需手动处理类型断言。
//	大小写不敏感：
//		自动将操作符转为小写 (strings.ToLower)，兼容多种输入风格。

func (c *universalFilterCondition) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Field    string `json:"field,omitempty"`
		Operator string `json:"operator"`
		Value    any    `json:"value,omitempty"`
	}

	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	c.Field = aux.Field
	c.Operator = strings.ToLower(aux.Operator)

	// Handle logical operators (and/or) - Value should be []*UniversalFilterCondition
	if c.Operator == operatorAnd || c.Operator == operatorOr {
		// Value can be an array of conditions
		valueSlice, ok := aux.Value.([]any)
		if !ok {
			return fmt.Errorf("logical operator %s requires an array of conditions", c.Operator)
		}

		conditions := make([]*universalFilterCondition, 0, len(valueSlice))
		for i, v := range valueSlice {
			condBytes, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("failed to marshal condition at index %d: %w", i, err)
			}

			var cond universalFilterCondition
			if err := json.Unmarshal(condBytes, &cond); err != nil {
				return fmt.Errorf("failed to unmarshal condition at index %d: %w", i, err)
			}
			conditions = append(conditions, &cond)
		}
		c.Value = conditions
	} else {
		c.Value = aux.Value
	}

	return nil
}

// MarshalJSON implements custom JSON marshaling for UniversalFilterCondition.
func (c *universalFilterCondition) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Field    string `json:"field,omitempty"`
		Operator string `json:"operator"`
		Value    any    `json:"value,omitempty"`
	}

	return json.Marshal(&Alias{
		Field:    c.Field,
		Operator: c.Operator,
		Value:    c.Value,
	})
}
