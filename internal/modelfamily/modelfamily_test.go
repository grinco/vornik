package modelfamily

import "testing"

func TestFamily(t *testing.T) {
	cases := map[string]string{
		"glm-5.2:cloud":                               "zhipu",
		"nvidia.nemotron-nano-9b-v2":                  "nvidia",
		"openai.gpt-oss-20b-1:0":                      "openai",
		"gpt-oss:20b":                                 "openai",
		"claude-haiku-3-5":                            "anthropic",
		"us.anthropic.claude-3-5-haiku-20241022-v1:0": "anthropic",
		"qwen3.6:35b":                                 "alibaba",
		"google/gemma-4-26b-a4b-it-maas":              "google",
		"gemini-2.5-pro":                              "google",
		"deepseek-r1:70b":                             "deepseek",
		"mistral-small:24b":                           "mistral",
		"llama3.3:70b":                                "meta",
		"":                                            Unknown,
		"totally-made-up-model":                       Unknown,
		"novel-thing":                                 Unknown, // "nova" must not match "novel"
		"ollama-x":                                    Unknown, // "o1" must not match "ollama"
	}
	for id, want := range cases {
		if got := Family(id); got != want {
			t.Errorf("Family(%q) = %q, want %q", id, got, want)
		}
	}
}

// Test 37 (design §9): a same-family assistant/judge pair is refused; a
// different-family pair passes; an unclassifiable id fails closed.
func TestSameFamily(t *testing.T) {
	if !SameFamily("glm-5.2:cloud", "glm-4.5-air") {
		t.Error("two GLM ids must be the same family")
	}
	if SameFamily("glm-5.2:cloud", "nvidia.nemotron-nano-9b-v2") {
		t.Error("GLM vs nemotron must be different families")
	}
	if SameFamily("glm-5.2:cloud", "openai.gpt-oss-20b-1:0") {
		t.Error("GLM vs gpt-oss must be different families")
	}
	if !SameFamily("glm-5.2:cloud", "mystery-model") {
		t.Error("an unclassifiable id must fail CLOSED (reported as same family)")
	}
	if !SameFamily("", "") {
		t.Error("empty ids must fail closed")
	}
}
