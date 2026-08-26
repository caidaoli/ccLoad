package app

import (
	"sync"
	"testing"
)

// parseMultimodalFallbackModels 的归一化契约：key 小写 + 剥思考后缀，value 保留原始写法。
// json 边界（尺寸/条数/尾随数据）已在 admin_settings_validation_test.go 覆盖。
func TestParseMultimodalFallbackModels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty_sentinel", value: "", want: nil},
		{name: "null_sentinel", value: "null", want: nil},
		{name: "empty_object", value: "{}", want: nil},
		{
			name:  "plain_mapping",
			value: `{"gpt-5.6-luna":"gemini-3-pro"}`,
			want:  map[string]string{"gpt-5.6-luna": "gemini-3-pro"},
		},
		{
			name:  "key_normalized_case_and_suffix",
			value: `{"GPT-5.6-Luna(max)":"Gemini-3-Pro(max)"}`,
			want:  map[string]string{"gpt-5.6-luna": "Gemini-3-Pro(max)"},
		},
		{
			name:  "trims_both_sides",
			value: `{" gpt-5.6-luna ":" gemini-3-pro "}`,
			want:  map[string]string{"gpt-5.6-luna": "gemini-3-pro"},
		},
		{name: "reject_not_json", value: "not json", wantErr: true},
		{name: "reject_trailing_data", value: `{"a":"b"}{"c":"d"}`, wantErr: true},
		{name: "reject_blank_from", value: `{"":"x"}`, wantErr: true},
		{name: "reject_blank_to", value: `{"x":""}`, wantErr: true},
		{name: "reject_self_mapping", value: `{"gpt":"gpt"}`, wantErr: true},
		{name: "reject_self_after_normalize", value: `{"gpt(max)":"gpt"}`, wantErr: true},
		{name: "reject_duplicate_normalized_key", value: `{"Gpt":"a","gpt(max)":"b"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMultimodalFallbackModels(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMultimodalFallbackModels(%q) err=nil, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMultimodalFallbackModels(%q) err=%v, want nil", tt.value, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseMultimodalFallbackModels(%q)=%v, want %v", tt.value, got, tt.want)
			}
			for key, value := range tt.want {
				if got[key] != value {
					t.Fatalf("parseMultimodalFallbackModels(%q)[%q]=%q, want %q", tt.value, key, got[key], value)
				}
			}
		})
	}
}

func TestServerMultimodalFallbackModel(t *testing.T) {
	t.Parallel()

	server := &Server{}
	server.setMultimodalFallbackModels(map[string]string{
		"gpt-text": "gpt-vision",
	})

	// 命中：请求模型带大小写与思考后缀也能归一到 key。
	if got := server.multimodalFallbackModel("GPT-Text(max)", true); got != "gpt-vision" {
		t.Fatalf("multimodalFallbackModel(GPT-Text(max), true)=%q, want gpt-vision", got)
	}
	// 未命中映射：原样返回空串（不改写）。
	if got := server.multimodalFallbackModel("gpt-other", true); got != "" {
		t.Fatalf("multimodalFallbackModel(gpt-other, true)=%q, want empty", got)
	}
	// 纯文本请求（hasNonText=false）即使命中也不改写。
	if got := server.multimodalFallbackModel("gpt-text", false); got != "" {
		t.Fatalf("multimodalFallbackModel(gpt-text, false)=%q, want empty", got)
	}
	// 未配置任何映射时直接短路。
	if got := (&Server{}).multimodalFallbackModel("gpt-text", true); got != "" {
		t.Fatalf("unconfigured multimodalFallbackModel=%q, want empty", got)
	}
}

func TestServerMultimodalFallbackModelHotSwap(t *testing.T) {
	t.Parallel()

	server := &Server{}
	first := map[string]string{"gpt-text": "gpt-vision-v1"}
	server.setMultimodalFallbackModels(first)
	first["gpt-text"] = "mutated-after-publish"
	if got := server.multimodalFallbackModel("gpt-text", true); got != "gpt-vision-v1" {
		t.Fatalf("published snapshot changed through caller map: got %q", got)
	}

	server.setMultimodalFallbackModels(map[string]string{"gpt-text": "gpt-vision-v2"})
	if got := server.multimodalFallbackModel("gpt-text", true); got != "gpt-vision-v2" {
		t.Fatalf("hot-swapped fallback=%q, want gpt-vision-v2", got)
	}

	server.setMultimodalFallbackModels(nil)
	if got := server.multimodalFallbackModel("gpt-text", true); got != "" {
		t.Fatalf("cleared fallback=%q, want empty", got)
	}
}

func TestServerMultimodalFallbackModelConcurrentHotSwap(t *testing.T) {
	t.Parallel()

	server := &Server{}
	server.setMultimodalFallbackModels(map[string]string{"gpt-text": "vision-a"})

	const iterations = 1000
	start := make(chan struct{})
	unexpected := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			fallback := "vision-a"
			if i%2 == 1 {
				fallback = "vision-b"
			}
			server.setMultimodalFallbackModels(map[string]string{"gpt-text": fallback})
		}
	}()
	for range 4 {
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				got := server.multimodalFallbackModel("gpt-text", true)
				if got != "vision-a" && got != "vision-b" {
					select {
					case unexpected <- got:
					default:
					}
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	select {
	case got := <-unexpected:
		t.Fatalf("concurrent lookup observed partial snapshot %q", got)
	default:
	}
}
