package llmserve

import "runtime/debug"

// codeHash is stamped by build-daemon.sh (-ldflags -X) with the output of
// scripts/llm-proxy-hash.sh: a hash over the source of this package and the
// packages of this module it imports, the versions of the modules it
// depends on, and the Go version. It moves when the service's own code
// moves and at no other time, which is what lets a deploy leave the service
// running.
var codeHash string

// CodeHash names the code this binary's proxy service is made of. The daemon
// compares its own with the running service's and restarts the service when
// they differ.
//
// A build nobody stamped (a release build, `go build` by hand) answers with
// its git commit instead: it cannot say whether the proxy changed, so every
// new commit counts as a change and the service restarts with the daemon,
// as it did before it was a service. A build with no commit either (a test
// binary) is "unstamped", and equals every other one.
func CodeHash() string {
	if codeHash != "" {
		return codeHash
	}
	return unstampedHash(debug.ReadBuildInfo())
}

func unstampedHash(info *debug.BuildInfo, ok bool) string {
	if !ok {
		return "unstamped"
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision":
			rev = s.Value
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "+dirty"
		}
	}
	if rev == "" {
		return "unstamped"
	}
	return "commit:" + rev + dirty
}
