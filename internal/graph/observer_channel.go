package graph

// ObserverCallable reports whether a node can participate in an Observer channel.
func ObserverCallable(n *Node) bool {
	return n != nil && (n.Kind == KindMethod || n.Kind == KindFunction)
}

// ObserverCallbackKinds returns the reference edge kinds used to find callbacks.
func ObserverCallbackKinds() []EdgeKind {
	return []EdgeKind{EdgeReads, EdgeReferences}
}

// ObserverCallbackAtSite checks whether a reference names a target at the call site.
func ObserverCallbackAtSite(call, ref *Edge) bool {
	return ref != nil && ref.To != "" && ref.FilePath == call.FilePath && ref.Line == call.Line
}

// ObserverCallbackCountAllowed applies the fan-out cap before boundary filtering.
func ObserverCallbackCountAllowed(count, limit int) bool {
	return count > 0 && count <= limit
}

// ObserverCallbackIsDistinct reports whether dispatch would be a non-self Call.
func ObserverCallbackIsDistinct(dispatcher, callback string) bool {
	return dispatcher != callback
}
