package shellint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteZdotdir_NoBinDir keeps the default shape untouched: a pane with no
// shim directory must get exactly the four proxy dotfiles and no PATH edit.
func TestWriteZdotdir_NoBinDir(t *testing.T) {
	dir := t.TempDir()
	if err := WriteZdotdir(dir, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".zshenv", ".zprofile", ".zshrc", ".zlogin"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "export PATH=") {
		t.Errorf("PATH touched with no bin dir:\n%s", b)
	}
}

// TestWriteZdotdir_PrependsPath runs a real zsh against the generated ZDOTDIR.
// Asserting on the file contents would prove nothing: the whole reason this
// lives in .zshrc rather than the pane's tmux environment is that a login shell
// rebuilds PATH afterwards, and only running one shows whether the entry
// survived — and survived FIRST, where it beats a real clipboard tool.
func TestWriteZdotdir_PrependsPath(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "clipbin")
	if err := WriteZdotdir(dir, binDir); err != nil {
		t.Fatal(err)
	}
	// -l so the profile files run, which is what reorders PATH on macOS.
	cmd := exec.Command(zsh, "-l", "-i", "-c", "print -r -- $PATH")
	cmd.Env = append(os.Environ(), "ZDOTDIR="+dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("zsh: %v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if first, _, _ := strings.Cut(got, string(os.PathListSeparator)); first != binDir {
		t.Errorf("PATH starts with %q, want the shim dir %q first\nfull: %s", first, binDir, got)
	}
}

// TestWriteZdotdir_KeepsUserZshrc pins that the PATH line is APPENDED. Prepending
// it would put the shim dir first only until the user's own .zshrc ran and put
// its own entries ahead of it.
func TestWriteZdotdir_KeepsUserZshrc(t *testing.T) {
	dir := t.TempDir()
	if err := WriteZdotdir(dir, "/rt/clipbin"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	src := strings.Index(body, `source "$HOME/.zshrc"`)
	path := strings.Index(body, "export PATH=")
	if src < 0 || path < 0 {
		t.Fatalf("zshrc missing source or PATH line:\n%s", body)
	}
	if path < src {
		t.Errorf("PATH line comes BEFORE the user's .zshrc — their config would win:\n%s", body)
	}
}
