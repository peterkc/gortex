package graph

// BuiltinReceiverHint holds raw, syntactic evidence. It is not derived link
// status. Hosts use the delegate language; edge coordinates remain the host's.
type BuiltinReceiverHint struct {
	Language string
	Form     string
	Family   string
	Name     string
	Module   string
}

func (e *Edge) SetBuiltinReceiverHint(h BuiltinReceiverHint) {
	if e.Kind != EdgeCalls || h.Form == "unknown" {
		return
	}
	if e.Meta == nil {
		e.Meta = make(map[string]any, 5)
	}
	e.Meta["platform_receiver_language"] = h.Language
	e.Meta["platform_receiver_form"] = h.Form
	setOptional := func(key, value string) {
		if value == "" {
			delete(e.Meta, key)
		} else {
			e.Meta[key] = value
		}
	}
	setOptional("platform_receiver_family", h.Family)
	setOptional("platform_receiver_name", h.Name)
	setOptional("platform_receiver_module", h.Module)
}

func (e *Edge) BuiltinReceiverHint() (BuiltinReceiverHint, bool) {
	if e.Kind != EdgeCalls {
		return BuiltinReceiverHint{}, false
	}
	lang, _ := e.Meta["platform_receiver_language"].(string)
	form, _ := e.Meta["platform_receiver_form"].(string)
	family, _ := e.Meta["platform_receiver_family"].(string)
	name, _ := e.Meta["platform_receiver_name"].(string)
	module, _ := e.Meta["platform_receiver_module"].(string)
	return BuiltinReceiverHint{Language: lang, Form: form, Family: family, Name: name, Module: module}, lang != "" && form != ""
}
