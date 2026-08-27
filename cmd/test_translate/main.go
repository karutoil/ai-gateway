package main

import (
	"ai-gateway/internal/translate"
	"encoding/json"
	"fmt"
)

func main() {
	openAI := `{"model":"muse-spark-1.2-contributor","messages":[{"role":"user","content":"What is weather in Paris? You must call get_weather."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}}],"tool_choice":"auto","max_tokens":200}`
	out, _, err := translate.OpenAIToAnthropic([]byte(openAI))
	if err != nil {
		fmt.Println("err", err)
		return
	}
	var m map[string]interface{}
	json.Unmarshal(out, &m)
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println("Translated OpenAI->Anthropic:")
	fmt.Println(string(b))
	fmt.Println("---")
	anthropic := `{"model":"muse-spark-1.2-contributor","max_tokens":200,"messages":[{"role":"user","content":"What is weather in Paris? You must call get_weather."}],"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}]}`
	fmt.Println("Direct Anthropic:")
	var m2 map[string]interface{}
	json.Unmarshal([]byte(anthropic), &m2)
	b2, _ := json.MarshalIndent(m2, "", "  ")
	fmt.Println(string(b2))
}
