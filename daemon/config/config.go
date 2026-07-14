// Package config embeds the managed tmux configuration so ccmuxd ships as a
// single self-contained binary (it writes this to a temp file at startup and
// points `tmux -L ccmux -f` at it).
package config

import _ "embed"

//go:embed tmux.conf
var TmuxConf string
