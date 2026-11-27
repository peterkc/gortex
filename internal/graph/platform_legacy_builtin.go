package graph

var legacyJSBuiltinsV1 = map[string]string{
	// Array
	"push": "array", "pop": "array", "shift": "array", "unshift": "array",
	"splice": "array", "reverse": "array", "sort": "array",
	"map": "array", "filter": "array", "reduce": "array", "reduceRight": "array",
	"forEach": "array", "findIndex": "array", "some": "array", "every": "array",
	"flat": "array", "flatMap": "array", "fill": "array", "copyWithin": "array",

	// Shared (array / string)
	"concat": "array/string", "slice": "array/string",
	"includes": "array/string", "indexOf": "array/string", "lastIndexOf": "array/string",
	"find": "array/string",

	// Array static
	"from": "array.static", "isArray": "array.static", "of": "array.static",

	// String
	"charAt": "string", "charCodeAt": "string", "codePointAt": "string",
	"startsWith": "string", "endsWith": "string",
	"match": "string", "matchAll": "string", "normalize": "string",
	"padStart": "string", "padEnd": "string", "repeat": "string",
	"replace": "string", "replaceAll": "string", "search": "string",
	"split": "string", "substring": "string", "substr": "string",
	"toLowerCase": "string", "toUpperCase": "string",
	"trim": "string", "trimStart": "string", "trimEnd": "string",
	"localeCompare": "string", "at": "string",
	"fromCharCode": "string.static", "fromCodePoint": "string.static",

	// Number
	"toFixed": "number", "toPrecision": "number", "toExponential": "number",
	"parseFloat": "number.static", "parseInt": "number.static",
	"isNaN": "number.static", "isFinite": "number.static", "isInteger": "number.static",

	// JSON (static on the JSON object)
	"parse": "json", "stringify": "json",

	// Object (static on Object)
	"assign": "object", "freeze": "object", "create": "object",
	"getPrototypeOf": "object", "setPrototypeOf": "object",
	"defineProperty": "object", "defineProperties": "object",

	// Map / Set / WeakMap / WeakSet
	"set": "map", "get": "map",
	"has": "map/set", "delete": "map/set", "clear": "map/set", "size": "map/set",
	"add": "set",

	// Shared (array / map / object / set): keys/values/entries
	"keys": "array/map/object", "values": "array/map/object", "entries": "array/map/object",

	// Promise
	"then": "promise", "catch": "promise", "finally": "promise",
	"resolve": "promise.static", "reject": "promise.static",
	"all": "promise.static", "allSettled": "promise.static", "race": "promise.static", "any": "promise.static",

	// DOM — Node / Element
	"querySelector": "dom", "querySelectorAll": "dom",
	"getElementById": "dom", "getElementsByClassName": "dom", "getElementsByTagName": "dom",
	"getAttribute": "dom", "setAttribute": "dom", "removeAttribute": "dom", "hasAttribute": "dom",
	"appendChild": "dom", "removeChild": "dom", "insertBefore": "dom",
	"replaceChild": "dom", "cloneNode": "dom",
	"addEventListener": "dom", "removeEventListener": "dom", "dispatchEvent": "dom",
	"click": "dom", "focus": "dom", "blur": "dom",
	"createElement": "dom", "createTextNode": "dom", "createDocumentFragment": "dom",
	"parseFromString": "dom.DOMParser",
	"contains":        "dom/array/string",

	// console
	"log": "console", "warn": "console", "error": "console", "debug": "console", "info": "console",
}

var legacyPythonBuiltinsV1 = map[string]string{
	// str
	"startswith": "str", "endswith": "str", "rfind": "str",
	"upper": "str", "lower": "str", "capitalize": "str", "title": "str",
	"strip": "str", "lstrip": "str", "rstrip": "str",
	"format": "str",

	// list
	"append": "list", "extend": "list", "insert": "list",

	// dict
	"setdefault": "dict", "update": "dict",

	// set
	"discard": "set", "union": "set", "intersection": "set",
	"difference": "set", "symmetric_difference": "set",

	// Shared across str/list/dict/set
	"split": "str", "rsplit": "str", "join": "str",
	"replace": "str",
	"remove":  "list/set", "pop": "list/dict/set",
	"index": "list/str", "count": "list/str", "sort": "list", "reverse": "list",
	"get": "dict", "keys": "dict", "values": "dict", "items": "dict",
	"add": "set", "has": "set",
	"find": "str",
}

// LegacyBuiltinCategory returns the native name-only category, not an
// exact receiver match. The existing native classifier keeps this V1 mapping.
func LegacyBuiltinCategory(method, lang string) (string, bool) {
	switch lang {
	case "js", "ts":
		c, ok := legacyJSBuiltinsV1[method]
		return c, ok
	case "py":
		c, ok := legacyPythonBuiltinsV1[method]
		return c, ok
	}
	return "", false
}
