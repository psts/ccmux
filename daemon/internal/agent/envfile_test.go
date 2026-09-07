package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFileIsDataNotShell(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte("# comment\n\nTOKEN=abc$(rm -rf /)\nexport NAME=\"two words\"\nSINGLE='it''s'\nSPACED = padded \nbad line\n1BAD=x\nrm -rf /\nPATH=/evil\nLD_PRELOAD=/evil.so\nCLAUDE_PEERS_NAME=other\nHOME=/tmp/x\nXDG_CONFIG_HOME=/tmp/x\nCCMUX_PEERS_URL=http://evil\nCCMUX_CMD_FILE=/tmp/ccmux-cmd-victim\n"), 0o600)
	vars, problems, err := ParseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"TOKEN": "abc$(rm -rf /)", "NAME": "two words", "SINGLE": "it''s", "SPACED": "padded"}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
	}
	if len(vars) != len(want) {
		t.Errorf("vars = %v", vars)
	}
	if len(problems) != 10 {
		t.Errorf("problems = %v, want the three bad lines and the seven refused keys named", problems)
	}
	for _, k := range []string{"PATH", "LD_PRELOAD", "CLAUDE_PEERS_NAME", "HOME", "XDG_CONFIG_HOME", "CCMUX_PEERS_URL", "CCMUX_CMD_FILE"} {
		if _, set := vars[k]; set {
			t.Errorf("%s must never come from a .env", k)
		}
	}
	if v, probs, err := ParseEnvFile(filepath.Join(dir, "missing")); err != nil || v != nil || probs != nil {
		t.Errorf("missing file = %v %v %v, want nothing", v, probs, err)
	}
}

func TestLoadEnvFilesLaterWins(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("X=1\nY=1\n"), 0o600)
	os.WriteFile(b, []byte("Y=2\nZ=2\n"), 0o600)
	vars, problems, err := LoadEnvFiles([]string{a, b, filepath.Join(dir, "none")})
	if err != nil || len(problems) != 0 || vars["X"] != "1" || vars["Y"] != "2" || vars["Z"] != "2" {
		t.Fatalf("vars %v problems %v err %v", vars, problems, err)
	}
}
