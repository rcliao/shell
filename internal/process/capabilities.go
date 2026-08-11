package process

// What a runtime can actually do.
//
// Until now the answer was discovered by type assertion — `agent.(Injector)`
// in absorb.go — or by assuming, since there was only ever one implementation.
// Assertion works for one optional method and stops working the moment a
// second runtime supports a different subset: the bridge would have to assert
// its way through a matrix, and a runtime that silently lacked image support
// would fail at use rather than at wiring.
//
// Declared instead. A runtime states what it can do; callers branch on that
// and degrade on purpose.
type Capabilities struct {
	// Images and PDFs report whether attachments can be sent at all. A runtime
	// without them should get a described attachment, not a dropped one — the
	// bridge already archives and describes photos, so the fallback exists.
	Images bool
	PDFs   bool
	// Streaming reports whether text arrives incrementally. Without it the
	// Telegram path should skip its placeholder-editing loop rather than
	// leave "Thinking..." on screen until the end.
	Streaming bool
	// Injection replaces the Injector type assertion: whether a message can be
	// folded into a turn already running (V2-H46 absorb).
	Injection bool
	// Models lists selectable model identifiers, empty when the runtime offers
	// only whatever it was configured with. Deliberately advisory: the CLI
	// accepts any string it is handed, so this documents intent rather than
	// constraining a call.
	Models []string
}

// Capabilities reports what the Claude CLI runtime supports.
//
// Everything is true because this runtime does everything shell currently
// asks of it. The value is not the answer today — it is that a second runtime
// can answer differently without the bridge learning about it by crashing.
func (m *Manager) Capabilities() Capabilities {
	return Capabilities{
		Images:    true,
		PDFs:      true,
		Streaming: true,
		Injection: true,
		Models:    nil, // the CLI takes any --model string; nothing to enumerate
	}
}
