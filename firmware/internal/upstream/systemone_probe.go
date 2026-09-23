package upstream

import (
	"encoding/json"
	"math"
)

// SystemOneProbeQuestion contains only validated, bounded results of fixed test
// questions. Never return arbitrary upstream strings or record answers in logs.
type SystemOneProbeQuestion struct {
	Type    string   `json:"type"`
	OK      bool     `json:"ok"`
	Message string   `json:"message" i18n:"text"`
	Choice  string   `json:"choice,omitempty"`
	Noul    *float64 `json:"noul,omitempty"`
	Score   *float64 `json:"score,omitempty"`
}

func systemOneProbeCriteria() []string {
	return []string{"可以等待", "本周处理", "今天处理"}
}

func systemOneProbePayload(model string) map[string]any {
	return map[string]any{
		"model": model,
		"state": "客户要求退款，并明确要求今天处理。",
		"questions": map[string]any{
			"choice": map[string]any{"type": "choice", "instructions": "应由哪个团队处理？", "criteria": map[string]string{"billing": "账单和退款", "technical": "技术故障"}},
			"noul":   map[string]any{"type": "noul", "instructions": "是否明确要求今天处理？", "criteria": map[string]string{"true": "明确要求今天处理", "false": "没有要求今天处理"}},
			"score":  map[string]any{"type": "score", "instructions": "评估紧急程度。", "criteria": systemOneProbeCriteria()},
		},
	}
}

func systemOneProbeResults(raw json.RawMessage) ([]SystemOneProbeQuestion, bool) {
	var answers map[string]json.RawMessage
	_ = json.Unmarshal(raw, &answers)
	results := make([]SystemOneProbeQuestion, 0, 3)
	allOK := len(answers) == 3
	for _, kind := range []string{"choice", "noul", "score"} {
		result := checkSystemOneProbeAnswer(kind, answers[kind])
		allOK = allOK && result.OK
		results = append(results, result)
	}
	return results, allOK
}

func checkSystemOneProbeAnswer(kind string, raw json.RawMessage) SystemOneProbeQuestion {
	result := SystemOneProbeQuestion{Type: kind, Message: "缺少有效的答案或答案类型不匹配"}
	var answer struct {
		Type          string              `json:"type"`
		Choice        string              `json:"choice"`
		Noul          *float64            `json:"noul"`
		Score         *float64            `json:"score"`
		Probabilities map[string]*float64 `json:"probabilities"`
		Confidence    *float64            `json:"confidence"`
		Legend        map[string]string   `json:"legend"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.Type != kind {
		return result
	}
	if kind == "noul" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		_, confidencePresent := fields["confidence"]
		if !probeUnitInterval(answer.Noul) || confidencePresent {
			result.Message = "Noul 必须为 0–1 的概率且不含 confidence"
			return result
		}
		result.Noul = answer.Noul
	} else {
		keys := []string{"billing", "technical"}
		if kind == "score" {
			keys = []string{"0", "1", "2"}
		}
		if !probeProbabilities(answer.Probabilities, keys) || !probeUnitInterval(answer.Confidence) {
			result.Message = "概率分布或 confidence 不合法"
			return result
		}
		if kind == "choice" {
			selected, ok := answer.Probabilities[answer.Choice]
			if !ok {
				result.Message = "Choice 未返回有效选项"
				return result
			}
			for _, probability := range answer.Probabilities {
				if *selected < *probability {
					result.Message = "Choice 与最高概率选项不一致"
					return result
				}
			}
			result.Choice = answer.Choice
		} else {
			weighted := 0.0
			for i, key := range keys {
				weighted += float64(i) * (*answer.Probabilities[key])
			}
			if answer.Score == nil || math.IsNaN(*answer.Score) || math.IsInf(*answer.Score, 0) || *answer.Score < 0 || *answer.Score > 2 || math.Abs(*answer.Score-weighted) > 0.0001 {
				result.Message = "Score 与概率加权分数不一致"
				return result
			}
			if len(answer.Legend) != len(keys) {
				result.Message = "Score legend 与请求等级不一致"
				return result
			}
			for i, description := range systemOneProbeCriteria() {
				if answer.Legend[keys[i]] != description {
					result.Message = "Score legend 与请求等级不一致"
					return result
				}
			}
			result.Score = answer.Score
		}
	}
	result.OK = true
	result.Message = ""
	return result
}

func probeUnitInterval(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= 1
}

func probeProbabilities(probabilities map[string]*float64, keys []string) bool {
	if len(probabilities) != len(keys) {
		return false
	}
	sum := 0.0
	for _, key := range keys {
		value, ok := probabilities[key]
		if !ok || !probeUnitInterval(value) {
			return false
		}
		sum += *value
	}
	return math.Abs(sum-1) <= 0.0001
}
