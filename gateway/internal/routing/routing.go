// 本包集中决定聊天请求使用的模型，路由结果会写入 trace 供回放核对。
package routing

import "strings"

// Rules 定义默认模型、任务分级模型及上下文长度阈值。
type Rules struct {
	DefaultModel     string
	SimpleModel      string
	ComplexModel     string
	ContextThreshold int
}

// Decision 记录模型选择结果及可回放的决策依据。
type Decision struct {
	Model         string
	TaskType      string
	ContextLength int
	Reason        string
}

// Select 按任务类型和上下文长度选择模型；长上下文优先于 simple 标签。
func Select(taskType string, contextLength int, requestedModel string, rules Rules) Decision {
	taskType = strings.TrimSpace(strings.ToLower(taskType))
	defaultModel := strings.TrimSpace(rules.DefaultModel)
	if defaultModel == "" {
		defaultModel = requestedModel
	}
	simpleModel := strings.TrimSpace(rules.SimpleModel)
	if simpleModel == "" {
		simpleModel = defaultModel
	}
	complexModel := strings.TrimSpace(rules.ComplexModel)
	if complexModel == "" {
		complexModel = defaultModel
	}

	decision := Decision{
		Model:         defaultModel,
		TaskType:      taskType,
		ContextLength: contextLength,
		Reason:        "default",
	}
	if taskType == "complex" {
		decision.Model = complexModel
		decision.Reason = "complex_task"
	} else if rules.ContextThreshold > 0 && contextLength >= rules.ContextThreshold {
		decision.Model = complexModel
		decision.Reason = "context_threshold"
	} else if taskType == "simple" {
		decision.Model = simpleModel
		decision.Reason = "simple_task"
	}
	return decision
}
