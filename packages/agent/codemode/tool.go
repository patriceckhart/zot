// Package codemode runs JavaScript that orchestrates tools in an isolated VM.
package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	executor "github.com/patriceckhart/zot/packages/codemode"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// ModelCaller implements the model catalog and non-chat model operations.
// Credentials and provider wire behavior stay in the host, never in the VM.
type ModelCaller func(context.Context, string, json.RawMessage) (json.RawMessage, *provider.Usage, error)

type Tool struct {
	Mode         string
	InlineBudget *int
	Models       ModelCaller
}

func (*Tool) Name() string { return "codemode" }
func (*Tool) Description() string {
	return "Run JavaScript that calls other tools. Provide raw JavaScript as an async function body, with top-level await and return, no markdown fences. tools.<name>(args) returns a promise. text(value), console.*, return and image(value) emit output. exit() succeeds immediately. store(key,value)/load(key) keep small JSON values across calls. ALL_TOOLS, searchTools(query,{limit?,namespace?}), describeTool(name), describeNamespace(name) discover tools and TypeScript declarations. Optional first line: // @options: {\"max_output_tokens\":10000,\"timeout_ms\":60000}. No deadline by default. No direct file, network, process or timer APIs. Nested calls retain guards and confirmation. Completed side effects are not rolled back."
}
func (*Tool) ConstrainedSampling() *provider.ConstrainedSampling {
	return &provider.ConstrainedSampling{Type: "grammar", InputProperty: "code", Variants: map[string]string{"openai_lark": SourceGrammar}}
}

// SourceGrammar constrains the optional first-line directive, leaving JavaScript
// syntax and option validation to the executor.
const SourceGrammar = `start: options_source | plain_source
options_source: OPTIONS_LINE NEWLINE SOURCE
plain_source: SOURCE

OPTIONS_LINE: /[ \t]*\/\/ @options:[^\r\n]*/
NEWLINE: /\r?\n/
SOURCE: /[\s\S]+/
`

func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Raw JavaScript source, optionally starting with // @options: {...}"}},"required":["code"],"additionalProperties":false}`)
}

// PresentTools changes only model-facing definitions, never the callable registry.
func (t *Tool) PresentTools(specs []provider.Tool) []provider.Tool {
	var result []provider.Tool
	var inline []provider.Tool
	for _, spec := range specs {
		if spec.Name == "codemode" {
			continue
		}
		if spec.Exposure == "model-only" {
			result = append(result, spec)
			continue
		}
		if !isDeferred(spec) && (t.Mode == "only" || spec.Exposure == "codemode") {
			inline = append(inline, spec)
		}
		if t.Mode != "only" {
			spec.Description += "\n\nCodemode: `tools." + executor.Identifier(spec.Name) + "(args)` resolves to " + schemaTypeOrString(spec.OutputSchema) + "."
		}
		if t.Mode != "only" || spec.Deferred {
			result = append(result, spec)
		}
	}
	budget := 3000
	if t.InlineBudget != nil {
		budget = *t.InlineBudget
	}
	var description strings.Builder
	description.WriteString(t.Description())
	if t.Models != nil {
		description.WriteString("\nmodels.getModelsOfType(type,provider?), getAvailableOfType(type,provider?), getModelOfType(type,provider,id), classify(model,{state,questions}) and generateImages(model,{input}) list or run classifier and image models. Chat models cannot run inside scripts. Model results carry stopReason and errorMessage. Show generated image blocks with image(block). Read codemode.md in zot's docs for the exact schemas.")
	}
	description.WriteString(inlineCatalog(inline, budget))
	for _, spec := range specs {
		if spec.Name == "codemode" {
			spec.Description = description.String()
			result = append(result, spec)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (*Tool) Exposure() string { return "model-only" }

func isDeferred(spec provider.Tool) bool {
	return spec.Exposure == "deferred" || spec.Exposure == "" && spec.Deferred
}

func schemaTypeOrString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "string"
	}
	return schemaType(raw)
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	source, err := parseSource(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	runtime := core.ToolRuntimeFromContext(ctx)
	attached := runtime != nil
	if !attached {
		runtime = &core.ToolRuntime{}
	}
	// Compile once before starting the script's own deadline. Caller cancellation
	// still bounds the execution, and every later invocation reuses compiled code.
	if err := executor.Initialize(); err != nil {
		return core.ToolResult{}, err
	}
	if source.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, source.Timeout)
		defer cancel()
	}
	started := time.Now()
	start := executor.Start{Code: source.Code, Models: attached && t.Models != nil, Store: map[string]json.RawMessage{}}
	if state := runtime.State["codemode"]; len(state) > 0 {
		if err := json.Unmarshal(state, &start.Store); err != nil {
			return core.ToolResult{}, fmt.Errorf("invalid codemode session state")
		}
	}
	callable := make([]provider.Tool, 0, len(runtime.Tools))
	for _, spec := range runtime.Tools {
		if spec.Name == "codemode" || spec.Exposure == "model-only" {
			continue
		}
		callable = append(callable, spec)
		start.Tools = append(start.Tools, executor.Tool{Name: spec.Name, Description: toolSample(spec), Namespace: spec.Namespace, Schema: spec.Schema, OutputSchema: spec.OutputSchema})
	}
	collector := outputCollector{limit: source.MaxOutputTokens * 4}
	var usage provider.Usage
	var usageMu sync.Mutex
	var usedModel bool
	generatedImages := 0
	modelSlots := make(chan struct{}, 4)
	result, vmErr := executor.Run(ctx, start, func(callCtx context.Context, kind, name string, args json.RawMessage) (json.RawMessage, error) {
		if kind == "global" {
			if !strings.HasPrefix(name, "models.") {
				return discoveryCall(name, args, callable)
			}
			if t.Models == nil || !attached {
				return nil, fmt.Errorf("model functions are unavailable")
			}
			var value json.RawMessage
			var spent *provider.Usage
			var err error
			if name == "models.classify" || name == "models.generateImages" {
				res := runtime.Run(callCtx, modelOperation{name: name, caller: t.Models, slots: modelSlots}, args)
				value, spent = res.StructuredContent, res.Usage
				if len(value) == 0 && res.IsError {
					var messages []string
					for _, block := range res.Content {
						if text, ok := block.(provider.TextBlock); ok {
							messages = append(messages, text.Text)
						}
					}
					err = fmt.Errorf("%s", strings.Join(messages, "\n"))
				}
			} else {
				value, spent, err = t.Models(callCtx, strings.TrimPrefix(name, "models."), args)
			}
			if name == "models.generateImages" && err == nil {
				var response struct {
					Output []struct {
						Type string `json:"type"`
					} `json:"output"`
				}
				if json.Unmarshal(value, &response) == nil {
					usageMu.Lock()
					for _, block := range response.Output {
						if block.Type == "image" {
							generatedImages++
						}
					}
					usageMu.Unlock()
				}
			}
			if spent != nil {
				usageMu.Lock()
				usage = usage.Add(*spent)
				usedModel = true
				usageMu.Unlock()
			}
			return value, err
		}
		var tool *provider.Tool
		for i := range callable {
			if callable[i].Name == name {
				tool = &callable[i]
				break
			}
		}
		if tool == nil {
			return nil, fmt.Errorf("unknown callable tool")
		}
		res := runtime.Call(callCtx, name, args)
		if len(tool.OutputSchema) > 0 && len(res.StructuredContent) > 0 {
			return res.StructuredContent, nil
		}
		var texts []string
		for _, content := range res.Content {
			if text, ok := content.(provider.TextBlock); ok {
				texts = append(texts, text.Text)
			}
		}
		text := strings.Join(texts, "\n")
		if res.IsError {
			if text == "" {
				text = "tool failed"
			}
			return nil, fmt.Errorf("%s", text)
		}
		return json.Marshal(text)
	}, collector.add)
	shown := false
	for _, item := range collector.items {
		if _, ok := item.(provider.ImageBlock); ok {
			shown = true
		}
	}
	status := ""
	if vmErr != nil {
		result.OK = false
		result.Error = vmErr.Error()
	}
	if ctx.Err() != nil {
		result.OK = false
		result.Error = ctx.Err().Error()
		status = "cancelled"
		if ctx.Err() == context.DeadlineExceeded {
			status = "timed_out"
		}
	}
	if !result.OK {
		collector.add(executor.Item{Type: "text", Text: "Script error:\n" + result.Error})
	}
	if generatedImages > 0 && !shown {
		noun := "image"
		if generatedImages != 1 {
			noun = "images"
		}
		collector.add(executor.Item{Type: "text", Text: fmt.Sprintf("Note: models.generateImages() returned %d %s that the script did not show. Show each image block of result.output with image(block).", generatedImages, noun)})
	}
	items, path := collector.finish()
	header := "Script completed"
	if !result.OK {
		header = "Script failed"
	}
	header += fmt.Sprintf("\nWall time %.1f seconds\nOutput:\n", time.Since(started).Seconds())
	out := core.ToolResult{Content: append([]provider.Content{provider.TextBlock{Text: header}}, items...), IsError: !result.OK, Status: status, Details: map[string]any{"fullOutputPath": path}}
	if result.OK {
		if result.Store == nil {
			result.Store = map[string]json.RawMessage{}
		}
		state, _ := json.Marshal(result.Store)
		if attached && result.StoreWritten {
			out.State = map[string]json.RawMessage{"codemode": state}
		}
	}
	usageMu.Lock()
	if usedModel {
		copy := usage
		out.Usage = &copy
	}
	usageMu.Unlock()
	return out, nil
}
