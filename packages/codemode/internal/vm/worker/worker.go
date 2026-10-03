// Package worker implements the JavaScript VM hosted inside WebAssembly.
package worker

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"

	"github.com/dop251/goja"
	"github.com/patriceckhart/zot/packages/codemode/internal/vm"
)

type pendingCall struct{ resolve, reject func(interface{}) error }

// Serve runs a single script. Only explicitly registered JSON capabilities can
// leave the interpreter. Output is streamed so a terminated VM keeps its prefix.
func Serve(input io.Reader, output io.Writer) error {
	decoder, encoder := json.NewDecoder(input), json.NewEncoder(output)
	var start codemodevm.Start
	if err := decoder.Decode(&start); err != nil {
		return err
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(4096)
	jsonObject := vm.Get("JSON").ToObject(vm)
	stringify, _ := goja.AssertFunction(jsonObject.Get("stringify"))
	parse, _ := goja.AssertFunction(jsonObject.Get("parse"))
	errorCtor, rangeErrorCtor := vm.Get("Error"), vm.Get("RangeError")
	formatters, err := vm.RunScript("codemode-globals.js", outputFormatter)
	if err != nil {
		return err
	}
	textFormat, _ := goja.AssertFunction(formatters.ToObject(vm).Get("text"))
	consoleFormat, _ := goja.AssertFunction(formatters.ToObject(vm).Get("format"))
	errorFormat, _ := goja.AssertFunction(formatters.ToObject(vm).Get("error"))
	var exitToken = &struct{}{}
	encode := func(v goja.Value) (json.RawMessage, error) {
		s, err := stringify(goja.Undefined(), v)
		if err != nil {
			return nil, err
		}
		if goja.IsUndefined(s) {
			return nil, fmt.Errorf("value is not JSON-serializable")
		}
		return json.RawMessage(s.String()), nil
	}
	decode := func(raw json.RawMessage) (goja.Value, error) {
		if len(raw) == 0 {
			return goja.Undefined(), nil
		}
		return parse(goja.Undefined(), vm.ToValue(string(raw)))
	}
	formatError := func(err error) string {
		if e, ok := err.(*goja.Exception); ok {
			v, conversionErr := errorFormat(goja.Undefined(), e.Value())
			if conversionErr != nil {
				return "could not format script exception"
			}
			return v.String()
		}
		return err.Error()
	}
	format := func(v goja.Value) (string, error) {
		s, err := textFormat(goja.Undefined(), v)
		if err != nil {
			return "", err
		}
		return s.String(), nil
	}
	write := func(frame codemodevm.Frame) {
		if err := encoder.Encode(frame); err != nil {
			vm.Interrupt(err)
		}
	}
	emit := func(v goja.Value) {
		s, err := format(v)
		if err != nil {
			panic(vm.NewTypeError("%s", formatError(err)))
		}
		write(codemodevm.Frame{Type: "output", Item: &codemodevm.Item{Type: "text", Text: s}})
	}
	_ = vm.Set("text", func(call goja.FunctionCall) goja.Value { emit(call.Argument(0)); return goja.Undefined() })
	console := vm.NewObject()
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(level, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, v := range call.Arguments {
				rendered, err := consoleFormat(goja.Undefined(), v)
				if err != nil {
					panic(vm.NewTypeError("%s", formatError(err)))
				}
				parts = append(parts, rendered.String())
			}
			write(codemodevm.Frame{Type: "output", Item: &codemodevm.Item{Type: "text", Text: strings.Join(parts, " ")}})
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)
	_ = vm.Set("image", func(call goja.FunctionCall) goja.Value {
		raw, err := encode(call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError("image requires a data URL or image block"))
		}
		item, err := imageItem(raw)
		if err != nil {
			panic(vm.NewTypeError("%s", err.Error()))
		}
		write(codemodevm.Frame{Type: "output", Item: &item})
		return goja.Undefined()
	})
	store := make(map[string]json.RawMessage, len(start.Store))
	for k, v := range start.Store {
		store[k] = v
	}
	storeWritten := false
	key := func(name string, v goja.Value) string {
		if _, ok := v.(goja.String); !ok {
			panic(vm.NewTypeError("%s() key must be a string", name))
		}
		return v.String()
	}
	_ = vm.Set("store", func(call goja.FunctionCall) goja.Value {
		k := key("store", call.Argument(0))
		if goja.IsUndefined(call.Argument(1)) {
			storeWritten = true
			delete(store, k)
			return goja.Undefined()
		}
		raw, err := encode(call.Argument(1))
		if err != nil {
			panic(vm.NewTypeError("store value is not JSON-serializable"))
		}
		chars := len(utf16.Encode([]rune(string(raw))))
		if chars > 262144 {
			exception, _ := vm.New(rangeErrorCtor, vm.ToValue(fmt.Sprintf("store(%q) value has %d characters of JSON, more than the limit of 262144. store() is for small state such as IDs or summaries.", k, chars)))
			panic(exception)
		}
		total := chars + len(utf16.Encode([]rune(k)))
		for other, value := range store {
			if other != k {
				total += len(utf16.Encode([]rune(other))) + len(utf16.Encode([]rune(string(value))))
			}
		}
		if total > 1048576 {
			exception, _ := vm.New(rangeErrorCtor, vm.ToValue("store is full: stored values would exceed 1048576 characters of JSON. Delete keys with store(key, undefined)."))
			panic(exception)
		}
		storeWritten = true
		store[k] = raw
		return goja.Undefined()
	})
	_ = vm.Set("load", func(call goja.FunctionCall) goja.Value {
		v, err := decode(store[key("load", call.Argument(0))])
		if err != nil {
			panic(vm.NewTypeError("invalid stored JSON"))
		}
		return v
	})
	_ = vm.Set("exit", func(goja.FunctionCall) goja.Value { vm.Interrupt(exitToken); return goja.Undefined() })
	pending := map[int]pendingCall{}
	sequence := 0
	callHost := func(kind, name string, args json.RawMessage) goja.Value {
		sequence++
		promise, resolve, reject := vm.NewPromise()
		pending[sequence] = pendingCall{resolve: resolve, reject: reject}
		write(codemodevm.Frame{Type: kind, ID: sequence, Name: name, Args: args})
		return vm.ToValue(promise)
	}
	rejectValue := func(err error) goja.Value {
		promise, _, reject := vm.NewPromise()
		if exception, ok := err.(*goja.Exception); ok {
			_ = reject(exception.Value())
		} else {
			_ = reject(vm.NewTypeError("%s", err.Error()))
		}
		return vm.ToValue(promise)
	}
	tools := vm.NewObject()
	_ = tools.SetPrototype(nil)
	catalog := make([]map[string]string, 0, len(start.Tools))
	for _, tool := range start.Tools {
		fn := func(call goja.FunctionCall) goja.Value {
			raw, err := encode(call.Argument(0))
			if err != nil {
				return rejectValue(err)
			}
			if len(raw) == 0 || raw[0] != '{' {
				return rejectValue(fmt.Errorf("tool arguments must be a JSON object"))
			}
			return callHost("tool", tool.Name, raw)
		}
		identifier := codemodevm.Identifier(tool.Name)
		if tools.Get(identifier) == nil {
			_ = tools.Set(identifier, fn)
			catalog = append(catalog, map[string]string{"name": identifier, "description": tool.Description})
		}
		if tools.Get(tool.Name) == nil {
			_ = tools.Set(tool.Name, fn)
		}
	}
	_ = vm.Set("tools", tools)
	catalogJSON, _ := json.Marshal(catalog)
	catalogValue, err := decode(catalogJSON)
	if err != nil {
		return err
	}
	_ = vm.Set("ALL_TOOLS", catalogValue)
	for _, name := range []string{"searchTools", "describeTool", "describeNamespace"} {
		_ = vm.Set(name, func(call goja.FunctionCall) goja.Value {
			values := vm.NewArray()
			for i, v := range call.Arguments {
				_ = values.Set(fmt.Sprint(i), v)
			}
			raw, err := encode(values)
			if err != nil {
				return rejectValue(err)
			}
			return callHost("global", name, raw)
		})
	}
	if start.Models {
		models := vm.NewObject()
		_ = models.SetPrototype(nil)
		for _, name := range []string{"getModelsOfType", "getAvailableOfType", "getModelOfType", "classify", "generateImages"} {
			_ = models.Set(name, func(call goja.FunctionCall) goja.Value {
				values := vm.NewArray()
				for i, v := range call.Arguments {
					_ = values.Set(fmt.Sprint(i), v)
				}
				raw, err := encode(values)
				if err != nil {
					return rejectValue(err)
				}
				return callHost("global", "models."+name, raw)
			})
		}
		_ = vm.Set("models", models)
	}
	if _, err := vm.RunScript("codemode-globals.js", protectGlobals); err != nil {
		return err
	}
	value, scriptErr := vm.RunScript("codemode.js", "(async function(){"+start.Code+"\n})()")
	var promise *goja.Promise
	if scriptErr == nil {
		promise, _ = value.Export().(*goja.Promise)
		if promise == nil {
			scriptErr = fmt.Errorf("script did not return a promise")
		}
	}
	for scriptErr == nil && promise.State() == goja.PromiseStatePending {
		if len(pending) == 0 {
			scriptErr = fmt.Errorf("script waits on a promise that cannot settle")
			break
		}
		var reply codemodevm.Frame
		if err := decoder.Decode(&reply); err != nil {
			scriptErr = err
			break
		}
		callback, ok := pending[reply.ID]
		if !ok {
			scriptErr = fmt.Errorf("unknown capability reply")
			break
		}
		delete(pending, reply.ID)
		if reply.OK {
			v, err := decode(reply.Value)
			if err != nil {
				scriptErr = err
			} else {
				scriptErr = callback.resolve(v)
			}
		} else {
			errObject, _ := vm.New(errorCtor, vm.ToValue(reply.Error))
			scriptErr = callback.reject(errObject)
		}
	}
	if interrupted, ok := scriptErr.(*goja.InterruptedError); ok && interrupted.Value() == exitToken {
		vm.ClearInterrupt()
		scriptErr = nil
		promise = nil
	}
	if scriptErr == nil && promise != nil {
		if promise.State() == goja.PromiseStateRejected {
			message, err := errorFormat(goja.Undefined(), promise.Result())
			if err != nil {
				scriptErr = err
			} else {
				scriptErr = fmt.Errorf("%s", message.String())
			}
		} else if !goja.IsUndefined(promise.Result()) {
			serialized, err := stringify(goja.Undefined(), promise.Result())
			if err != nil {
				scriptErr = err
			} else if !goja.IsUndefined(serialized) {
				value, err := decode(json.RawMessage(serialized.String()))
				if err != nil {
					scriptErr = err
				} else {
					s, err := format(value)
					if err != nil {
						scriptErr = err
					} else {
						write(codemodevm.Frame{Type: "output", Item: &codemodevm.Item{Type: "text", Text: s}})
					}
				}
			}
		}
	}
	result := codemodevm.Frame{Type: "done", OK: scriptErr == nil}
	if scriptErr == nil {
		result.Store = store
		result.StoreWritten = storeWritten
	} else {
		result.Error = formatError(scriptErr)
	}
	return encoder.Encode(result)
}

func imageItem(raw json.RawMessage) (codemodevm.Item, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return codemodevm.Item{}, err
	}
	var url string
	if s, ok := value.(string); ok {
		url = s
	} else if object, ok := value.(map[string]any); ok {
		if s, ok := object["image_url"].(string); ok {
			url = s
		} else if object["type"] == "image" {
			data, _ := object["data"].(string)
			if strings.HasPrefix(strings.ToLower(data), "data:") {
				url = data
			} else {
				url = "data:;base64," + data
			}
		}
	}
	if strings.HasPrefix(url, "http:") || strings.HasPrefix(url, "https:") {
		return codemodevm.Item{}, fmt.Errorf("remote image URLs are not supported")
	}
	header, data, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
		return codemodevm.Item{}, fmt.Errorf("image requires a base64 data URL or image block")
	}
	data = strings.Join(strings.Fields(data), "")
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) == 0 {
		return codemodevm.Item{}, fmt.Errorf("invalid image base64")
	}
	var mime string
	switch {
	case bytes.HasPrefix(decoded, []byte("\x89PNG\r\n\x1a\n")):
		mime = "image/png"
	case len(decoded) >= 4 && decoded[0] == 0xff && decoded[1] == 0xd8 && decoded[2] == 0xff && decoded[3] != 0xf7:
		mime = "image/jpeg"
	case bytes.HasPrefix(decoded, []byte("GIF87a")) || bytes.HasPrefix(decoded, []byte("GIF89a")):
		mime = "image/gif"
	case len(decoded) >= 12 && bytes.Equal(decoded[:4], []byte("RIFF")) && bytes.Equal(decoded[8:12], []byte("WEBP")):
		mime = "image/webp"
	default:
		return codemodevm.Item{}, fmt.Errorf("image data must be PNG, JPEG, GIF, or WebP")
	}
	return codemodevm.Item{Type: "image", MimeType: mime, Data: data}, nil
}
