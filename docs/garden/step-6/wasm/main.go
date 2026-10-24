// The site's WebAssembly module, built with TinyGo (npm run wasm). It runs
// in the visitor's browser and puts its functions on window.yggsite.
package main

import (
	"strings"
	"syscall/js"
	"unicode"
)

// stats counts a text's words and sentences, and how long it takes to read
// at about 200 words a minute.
func stats(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return nil
	}
	text := args[0].String()
	words := len(strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' }))
	sentences := strings.Count(text, ".") + strings.Count(text, "!") + strings.Count(text, "?")
	if sentences == 0 && words > 0 {
		sentences = 1
	}
	minutes := (words + 199) / 200
	return map[string]any{"words": words, "sentences": sentences, "minutes": minutes}
}

// slug turns a title into an address part: "Hello, World!" is hello-world.
func slug(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return ""
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(args[0].String()) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func main() {
	js.Global().Set("yggsite", js.ValueOf(map[string]any{
		"stats": js.FuncOf(stats),
		"slug":  js.FuncOf(slug),
	}))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("yggsite-ready"))
	select {} // keeps the functions alive
}
