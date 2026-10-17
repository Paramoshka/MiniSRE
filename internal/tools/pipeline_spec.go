package tools

import (
	"context"
	"encoding/json"
)

func pipelineDefinition(sources []Definition) Definition {
	spec := pipelineSpec(sources)
	return Definition{
		Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters,
		Run: func(ctx context.Context, t *Tools, raw json.RawMessage) (string, error) {
			return t.Pipeline(ctx, raw)
		},
	}
}

func pipelineSpec(sources []Definition) Spec {
	var variants []any
	for _, source := range sources {
		if source.Pipeline == nil {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(source.Parameters, &schema); err != nil {
			panic(err)
		}
		properties := schema["properties"].(map[string]any)
		delete(properties, "host_id")
		if source.Name == "get_top_processes" {
			properties["sort_by"] = map[string]any{
				"type":        "string",
				"enum":        []string{"memory", "cpu"},
				"description": "Sort by RSS or lifetime-average CPU; interval sampling is not available in pipelines.",
			}
		}
		if source.Name == "curl_http" {
			properties["download"] = map[string]any{"type": "boolean", "enum": []bool{false}, "description": "Use the pipeline stdout=file mode to save the body."}
		}
		variants = append(variants, map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"tool": map[string]any{"type": "string", "enum": []string{source.Name}}, "args": schema},
			"required":   []string{"tool"},
		})
	}
	filters := json.RawMessage(`{"type":"array","maxItems":4,"items":{"oneOf":[{"type":"object","properties":{"type":{"type":"string","enum":["grep"]},"pattern":{"type":"string","minLength":1},"regex":{"type":"boolean"},"ignore_case":{"type":"boolean"},"invert":{"type":"boolean","description":"Exclude matching lines; default false."},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Maximum matches; default 100."}},"required":["type","pattern"],"additionalProperties":false},{"type":"object","properties":{"type":{"type":"string","enum":["head","tail"]},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"Default 100."}},"required":["type"],"additionalProperties":false},{"type":"object","properties":{"type":{"type":"string","enum":["sed"]},"start_line":{"type":"integer","minimum":1,"description":"Default 1."},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"Default 100."}},"required":["type"],"additionalProperties":false},{"type":"object","properties":{"type":{"type":"string","enum":["count"]},"ignore_case":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":500,"description":"Number of most frequent lines; default 20."}},"required":["type"],"additionalProperties":false}]}}`)
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"source"},
		"properties": map[string]any{
			"host_id": map[string]any{"type": "string", "description": "One host for the whole pipeline; default local."},
			"source":  map[string]any{"oneOf": variants},
			"filters": filters,
			"stdout":  map[string]any{"type": "string", "enum": []string{"inline", "file", "discard"}, "description": "Default inline. Files are created in a new private /tmp/mini-sre-* directory on host_id."},
			"stderr":  map[string]any{"type": "string", "enum": []string{"inline", "file", "discard", "merge"}, "description": "Default inline; merge joins each stage's stderr into stdout before the next filter."},
		},
	}
	parameters, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return Spec{Name: "run_pipeline", Description: "Run one existing diagnostic source and up to four validated stdin filters (grep with optional inversion, head, tail, sed line range, or count of repeated lines) on one host, before limiting the preview. Uses raw source command output, not the standalone tool's formatted response. Always reports each stage's exit status; empty grep results and ICMP packet loss remain diagnostic results. Default stdout/stderr=inline; file saves raw output in a private /tmp/mini-sre-* directory, discard hides data but retains execution status. Artifacts may contain secrets. Whole operation has a 15-second timeout; files are capped at 10 MiB each. No arbitrary programs, shell strings, file paths, nested SSH or PTY.", Parameters: parameters}
}
