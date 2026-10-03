package worker

// Member guards improve script errors without exposing the host bridge. All
// intrinsics are captured before user code runs, and catalogs are immutable.
const protectGlobals = `(function() {
 const freeze = Object.freeze
 const define = Object.defineProperty
 const keys = Object.keys
 const get = Reflect.get
 const TypeErrorCtor = TypeError
 const comparable = name => name.toLowerCase().replace(/[^a-z0-9]/g, "")
 function guard(target, label, names, hint) {
  freeze(target)
  return new Proxy(target, {
   get(object, property, receiver) {
    if (typeof property !== "string" || property in object || property in Object.prototype || property === "then" || property === "toJSON") return get(object, property, receiver)
    const wanted = comparable(property)
    const exact = names.filter(name => comparable(name) === wanted)
    const close = exact.length ? exact : names.filter(name => wanted && (comparable(name).includes(wanted) || wanted.includes(comparable(name))))
    let message = label + "." + property + " does not exist."
    if (close.length) message += " Did you mean " + close.slice(0, 5).map(name => label + "." + name).join(", ") + "?"
    else if (names.length <= 20) message += " Available: " + names.join(", ") + "."
    if (hint) message += " " + hint
    message += ' Check for a member with "' + property + '" in ' + label + "."
    throw new TypeErrorCtor(message)
   }
  })
 }
 const catalog = globalThis.ALL_TOOLS
 for (const entry of catalog) freeze(entry)
 freeze(catalog)
 const toolProxy = guard(globalThis.tools, "tools", catalog.map(tool => tool.name), "ALL_TOOLS lists every tool, searchTools(query) finds tools by topic.")
 define(globalThis, "tools", {value: toolProxy, writable: false, configurable: false, enumerable: true})
 if (globalThis.models) {
  const models = guard(globalThis.models, "models", keys(globalThis.models))
  define(globalThis, "models", {value: models, writable: false, configurable: false, enumerable: true})
 }
 freeze(globalThis.console)
 for (const name of ["ALL_TOOLS", "console", "text", "image", "exit", "store", "load", "searchTools", "describeTool", "describeNamespace"]) define(globalThis, name, {value: globalThis[name], writable: false, configurable: false, enumerable: true})
})()`

const outputFormatter = `(function(stringify, ErrorCtor, StringCtor) {
 function errorText(value) {
  const head = value.message ? value.name + ": " + value.message : StringCtor(value.name)
  const stack = typeof value.stack === "string" ? value.stack.split("\n").filter(line => line.trim() && !line.includes("codemode-globals.js")) : []
  if (stack[0] === head) stack.shift()
  return [head, ...stack].join("\n")
 }
 function text(value) {
  if (value === undefined || value === null || typeof value !== "object" && typeof value !== "function") return StringCtor(value)
  const json = stringify(value)
  return json === undefined ? StringCtor(value) : json
 }
 function format(value) {
  if (typeof value === "string") return value
  if (value instanceof ErrorCtor) return errorText(value)
  try {
   const json = stringify(value)
   return json === undefined ? StringCtor(value) : json
  } catch { return StringCtor(value) }
 }
 return {text, format, error: value => value instanceof ErrorCtor ? errorText(value) : format(value)}
})(JSON.stringify, Error, String)`
