package main

import "time"

// Claude Code is not listening for channel messages the moment the handshake
// ends. It sends notifications/initialized first and puts its channel handler
// on the connection afterwards, from its own app code: a median 0.1s later and
// 1.6s at worst over 146 sessions (measured 2026-11-14 from its MCP logs,
// "Successfully connected" to "Channel notifications registered"). A push
// written in that gap is dropped without a word, while this process has
// already counted it as shown and acked it, so neither a replay nor
// check_messages brings it back.
//
// That gap is exactly where the message that STARTED a session lands: it is
// waiting when the push channel attaches, and the daemon replays it at once.
// Of 148 sessions started by a message in the thirty days before this, 2 got
// it; the rest sat out the birth prompt's minute and asked for a resend.
//
// Nothing tells a server that the handler is on, so two things stand in for
// it. The session's first tool call: only a model that is up and running can
// make one, and its app registered the handler long before the model could
// answer. And listenSettle after the handshake, for a session with nothing to
// call a tool about (mail waiting for a session a person opened and left).

// listenSettle is how long after the handshake a session that has called no
// tool is taken to be listening: several times the worst gap measured. It only
// ever delays the pushes of a session's first seconds.
var listenSettle = 10 * time.Second

// markClientListening opens the gate, for good. The tools/call handler calls
// it once the call is ANSWERED, not when it arrives: if that first call is
// check_messages, its poll hands the waiting mail over in the tool result, and
// the held push then finds it already shown instead of racing the poll to
// show it twice.
func (a *app) markClientListening() {
	if a.clientListening != nil {
		a.clientListeningOnce.Do(func() { close(a.clientListening) })
	}
}

// startListenSettle starts the clock that opens the gate for a session that
// calls no tool. Called when the handshake ends, once.
func (a *app) startListenSettle() {
	if a.clientListening != nil {
		a.listenSettleOnce.Do(func() { time.AfterFunc(listenSettle, a.markClientListening) })
	}
}

// waitClientListening holds a push until Claude Code can be taken to hear it:
// the session's first tool call or listenSettle after the handshake, whichever
// comes first. Free once the gate is open, and for an app built without one
// (tests).
func (a *app) waitClientListening() {
	if a.clientListening != nil {
		<-a.clientListening
	}
}
