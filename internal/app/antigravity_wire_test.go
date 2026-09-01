package app

import (
	"testing"

	"github.com/tidwall/gjson"
)

const antigravitySchemaTestModel = "gemini-3-pro"

func TestNormalizeAntigravitySchemasConsolidatesParametersAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parameters":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".parameters").IsObject() {
				t.Fatalf("parameters missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".parameters.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("parameters.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"parametersJsonSchema", "parameters_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesResponseAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"response":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".response").IsObject() {
				t.Fatalf("response missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".response.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("response.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesGenerationConfigSchemaAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"generationConfig":{
				"responseSchema":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"generationConfig":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
		{
			name: "generation_config alias carries schema aliases",
			input: `{"generation_config":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
				t.Fatalf("generationConfig.responseSchema missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("generationConfig.responseSchema.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
				if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
					t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
				}
			}
			if gjson.GetBytes(got, "generation_config").Exists() {
				t.Fatalf("generation_config parent should be removed; output=%s", got)
			}
		})
	}
}

func TestNormalizeAntigravitySchemasFoldsGenerationConfigMixedFields(t *testing.T) {
	t.Parallel()

	input := `{"generation_config":{
		"temperature":0.2,
		"response_json_schema":{"type":"object","properties":{"field":{"type":"string"}}}
	}}`
	got := normalizeAntigravitySchemas([]byte(input), antigravitySchemaTestModel)

	if gjson.GetBytes(got, "generation_config").Exists() {
		t.Fatalf("generation_config parent should be removed; output=%s", got)
	}
	if !gjson.GetBytes(got, "generationConfig").IsObject() {
		t.Fatalf("generationConfig missing: %s", got)
	}
	if gotTemp := gjson.GetBytes(got, "generationConfig.temperature").Float(); gotTemp != 0.2 {
		t.Fatalf("generationConfig.temperature = %v, want 0.2; output=%s", gotTemp, got)
	}
	if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
		t.Fatalf("generationConfig.responseSchema missing: %s", got)
	}
	if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties.field.type").String(); gotProp != "string" {
		t.Fatalf("generationConfig.responseSchema.properties.field.type = %q, want string; output=%s", gotProp, got)
	}
	for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
		if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
			t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
		}
	}
}
