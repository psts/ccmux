import { test } from "node:test";
import assert from "node:assert/strict";
import { lex, plainRead } from "./readcommands.mjs";

test("commands that only read are let through", () => {
  for (const line of [
    "ls -la src", "cat a.txt b.txt", "grep -rn 'needle > hay' src | head -20", "rg --files | wc -l",
    "git status", "git --no-pager log --oneline -5", "git -C /r/api diff main...HEAD -- src", "git show HEAD:README.md",
    "git branch -a", "git branch --show-current", "git remote -v", "git config --get user.name", "git worktree list", "git stash list",
    "find . -name '*.go' -newer go.mod", "sed -n '10,40p' main.go", "sort names.txt | uniq -c | sort -rn",
    "cat x 2>/dev/null", "ls missing 2>&1 | tail -1", "ls > /dev/null", "cd src && ls; pwd",
    "LC_ALL=C sort names.txt", "go list ./...", "go env GOPATH", "npm ls --depth=0", "node --version",
    "echo \"it's fine\"", "jq '.name' package.json", "test -f go.mod && echo yes || echo no",
    "rg -n --glob '*.go' needle", "ag needle src", "tree -L 2 -I node_modules", "file src/a.go", "fd -e go", "TZ=UTC date", "sort -t: -k2 names.txt",
    "ls 2>/dev/null && pwd", "ls src/*.go", "grep -n \"a\\\nb\" f", "ls 'my dir'/*.go", "rg -n foo -g '*.go'", "git remote -v show origin", "git grep -in needle", "git diff --exit-code", "yq -P '.a' f.yml", "find . -mtime -1 -size +10k -type f",
    "sort -rn -k2 -t : f", "sort --key=2 --reverse f", "tree -a -L 3 --dirsfirst", "sed -n '1,5p' f", "grep -n 'a$' f", "git config --get-all remote.origin.url",
    "git log --format='%h %s' \\\n  -3",
  ]) assert.equal(plainRead(line), true, line);
});

test("anything that can write, or run more than is written, is refused", () => {
  for (const line of [
    "touch made.txt", "rm -rf build", "echo hi > note.txt", "echo hi >> note.txt", "cat a 2> errors.txt", "ls &> all.txt",
    "ls >/dev/nullx", "tee out.txt", "cat a | tee out.txt", "npm test", "npm install", "go test ./...", "go env -w GOFLAGS=-mod=mod",
    "make", "python3 script.py", "node -e 'require(\"fs\").writeFileSync(\"x\",\"\")'", "bash -c 'ls'", "sh run.sh", "xargs rm",
    "ls $(cat files)", "ls `cat files`", "echo \"$(touch x)\"", "(cd src; ls)", "ls & ", "cat <<EOF\nhi\nEOF", "diff <(ls a) <(ls b)",
    "git commit -m x", "git checkout main", "git add .", "git fetch", "git pull", "git stash", "git stash pop", "git branch new-one",
    "git branch -D old", "git tag v1", "git config user.name me", "git -c core.pager=touch\\ x log", "git diff --output=patch.txt",
    "git worktree add ../x", "git reflog expire --all", "git remote add o url", "git grep -Oless needle",
    "find . -name '*.tmp' -delete", "find . -exec rm {} ;", "sed -i 's/a/b/' f", "sed 's/a/b/w out' f", "sed -n 'w out' f",
    "sort -o names.txt names.txt", "uniq in.txt out.txt", "awk '{print > \"out\"}' f", "awk 'BEGIN{system(\"touch x\")}'",
    "env FOO=1 make", "yq -i '.a=1' f.yml", "curl -o f https://example.com", "cat 'unclosed", "", "   ", "unknowncmd --help",
    // A variable in front can name a program to run.
    "GIT_EXTERNAL_DIFF=./x git diff", "LESSOPEN='|./x %s' cat f", "PAGER=./x git log", "FOO=1 ls",
    // Flags that run a program or write a file, alone, in a cluster, or with the value glued on.
    "rg --pre ./x needle", "rg --pre=./x needle", "ag --pager ./x needle", "tree -o out.txt", "tree -ao out.txt", "less -o log f", "more f",
    "sort -uo names.txt names.txt", "sort -onames.txt names.txt", "sort --output=names.txt names.txt", "fd -Hx rm", "file -C -m magic",
    "git config --get x --add y", "ls & pwd", "ls |& tee x", "cat a >| b", "echo \\",
    // The shell's own expansions, which can turn into a flag this file never
    // saw (2026-11-17 ship review): variables, brace lists, $'...', a word
    // that starts with a wildcard, and a flag split by backslash-newline.
    "echo $HOME", "X=-delete; find . $X", "LANG=-delete; find . $LANG", "COLUMNS=-delete", "printf -v X -- -delete; find . $X",
    "TERM=--output=x.txt; sort $TERM f", "find . -name x {-delete,}", "sort {-o,out.txt} f", "git diff {--output=x,}",
    "find . -delet{e,e}", "find . $'-delete'", "find . -del\\\nete", "find . \"\\\n-delete\"", "sort \"\\\n-ofile\" f", "find . *", "find . ''*", 'find . ""*', "find . '-'*", "sort ''* f", "ls \"$HOME\"", "cat ${f}",
    // Flags that write or run, found only by a list of the ones allowed.
    "tree -R -H . -L 1", "sort -S 1K --compress-program=sh f", "sort --comp=sh f", "sort -T /tmp f", "sed -n --i '1p' f",
    "git remote -v add o url", "git remote -v remove origin", "git remote --verbose set-url origin x", "git remote -v update",
    "git diff --outp=x", "git grep --open less needle", "git grep -iOless needle", "git diff --ext", "yq -Pi '.a=1' f.yml", "awk '{print $1}' log.txt", "awk -E prog f", "file --comp m", "find . -fprint out", "find . -okdir rm ;",
  ]) assert.equal(plainRead(line), false, JSON.stringify(line));
  assert.equal(plainRead(undefined), false);
});

test("the line is split the way a shell would split it", () => {
  assert.deepEqual(lex("grep -n 'a b' \"c d\" e\\ f | wc -l"), [["grep", "-n", "a b", "c d", "e f"], ["wc", "-l"]]);
  assert.deepEqual(lex("ls 2>/dev/null; pwd"), [["ls"], ["pwd"]]);
  assert.deepEqual(lex("a && b || c"), [["a"], ["b"], ["c"]]);
  assert.equal(lex("a > b"), null);
});
