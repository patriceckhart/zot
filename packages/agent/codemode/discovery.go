package codemode

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	executor "github.com/patriceckhart/zot/packages/codemode"
	"github.com/patriceckhart/zot/packages/provider"
)

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var acronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
var termsPattern = regexp.MustCompile(`[a-z0-9]+`)
var stopWords = map[string]bool{"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "for": true, "from": true, "in": true, "is": true, "it": true, "of": true, "on": true, "or": true, "that": true, "the": true, "this": true, "to": true, "with": true}

func tokenize(text string) []string {
	text = camelBoundary.ReplaceAllString(text, "$1 $2")
	text = acronymBoundary.ReplaceAllString(text, "$1 $2")
	var out []string
	for _, word := range termsPattern.FindAllString(strings.ToLower(text), -1) {
		if stopWords[word] {
			continue
		}
		switch {
		case len(word) > 4 && strings.HasSuffix(word, "ies"):
			word = word[:len(word)-3] + "y"
		case len(word) > 4 && (strings.HasSuffix(word, "ches") || strings.HasSuffix(word, "shes") || strings.HasSuffix(word, "sses") || strings.HasSuffix(word, "xes") || strings.HasSuffix(word, "zes")):
			word = word[:len(word)-2]
		case len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss"):
			word = word[:len(word)-1]
		}
		out = append(out, word)
	}
	return out
}

func schemaSearchText(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	if description, ok := object["description"].(string); ok {
		parts = append(parts, description)
	}
	if properties, ok := object["properties"].(map[string]any); ok {
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			parts = append(parts, name, schemaSearchText(properties[name]))
		}
	}
	parts = append(parts, schemaSearchText(object["items"]))
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if variants, ok := object[key].([]any); ok {
			for _, variant := range variants {
				parts = append(parts, schemaSearchText(variant))
			}
		}
	}
	return strings.Join(parts, " ")
}

func rankedTools(query string, tools []provider.Tool, limit int) []provider.Tool {
	seen := map[string]bool{}
	var queries []string
	for _, term := range tokenize(query) {
		if !seen[term] {
			seen[term] = true
			queries = append(queries, term)
		}
	}
	type document struct {
		tool   provider.Tool
		counts map[string]int
		length int
		score  float64
	}
	docs := make([]document, 0, len(tools))
	var total int
	for _, tool := range tools {
		var schema any
		_ = json.Unmarshal(tool.Schema, &schema)
		terms := tokenize(tool.Name + " " + strings.ReplaceAll(tool.Name, "_", " ") + " " + tool.Description + " " + schemaSearchText(schema) + " " + tool.Namespace + " " + tool.NamespaceDescription + " " + tool.NamespaceInstructions)
		counts := map[string]int{}
		for _, term := range terms {
			counts[term]++
		}
		docs = append(docs, document{tool: tool, counts: counts, length: len(terms)})
		total += len(terms)
	}
	if len(docs) == 0 {
		return nil
	}
	average := float64(total) / float64(len(docs))
	if average == 0 {
		average = 1
	}
	for _, term := range queries {
		frequency := 0
		for _, doc := range docs {
			if doc.counts[term] > 0 {
				frequency++
			}
		}
		idf := math.Log(1 + (float64(len(docs)-frequency)+0.5)/(float64(frequency)+0.5))
		for i := range docs {
			count := float64(docs[i].counts[term])
			if count == 0 {
				continue
			}
			norm := 1.2 * (1 - 0.75 + 0.75*float64(docs[i].length)/average)
			docs[i].score += idf * (count * 2.2) / (count + norm)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].score > docs[j].score })
	out := make([]provider.Tool, 0)
	for _, doc := range docs {
		if doc.score > 0 && len(out) < limit {
			out = append(out, doc.tool)
		}
	}
	return out
}

func discoveryCall(name string, raw json.RawMessage, tools []provider.Tool) (json.RawMessage, error) {
	var args []json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil || len(args) == 0 {
		return nil, fmt.Errorf("%s expects a string argument", name)
	}
	var query string
	if err := json.Unmarshal(args[0], &query); err != nil || string(args[0]) == "null" {
		return nil, fmt.Errorf("%s expects a string argument", name)
	}
	switch name {
	case "describeTool":
		for _, tool := range tools {
			if tool.Name == query || executor.Identifier(tool.Name) == query {
				return json.Marshal(toolSample(tool))
			}
		}
		return nil, nil
	case "describeNamespace":
		var matched []string
		var namespace, description, instructions string
		for _, tool := range tools {
			if tool.Namespace != "" && namespaceMatches(tool.Namespace, query) {
				if namespace == "" {
					namespace = tool.Namespace
					description, instructions = tool.NamespaceDescription, tool.NamespaceInstructions
				}
				matched = append(matched, executor.Identifier(tool.Name))
			}
		}
		if namespace == "" {
			return nil, nil
		}
		value := map[string]any{"name": namespace, "tools": matched}
		if description != "" {
			value["description"] = description
		}
		if instructions != "" {
			value["instructions"] = instructions
		}
		return json.Marshal(value)
	case "searchTools":
		options := struct {
			Limit     *int    `json:"limit"`
			Namespace *string `json:"namespace"`
		}{}
		if len(args) > 1 {
			if err := json.Unmarshal(args[1], &options); err != nil {
				return nil, fmt.Errorf("invalid searchTools options")
			}
		}
		limit := 8
		if options.Limit != nil {
			limit = *options.Limit
		}
		if limit <= 0 {
			return nil, fmt.Errorf("searchTools limit must be a positive integer")
		}
		filtered := make([]provider.Tool, 0, len(tools))
		for _, tool := range tools {
			if options.Namespace == nil || *options.Namespace == "" || tool.Namespace != "" && namespaceMatches(tool.Namespace, *options.Namespace) {
				filtered = append(filtered, tool)
			}
		}
		matches := make([]map[string]string, 0)
		for _, tool := range rankedTools(query, filtered, limit) {
			matches = append(matches, map[string]string{"name": executor.Identifier(tool.Name), "description": toolSample(tool)})
		}
		return json.Marshal(matches)
	default:
		return nil, fmt.Errorf("unknown discovery function")
	}
}
