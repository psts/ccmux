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
    "LC_ALL=C sort names.txt", "awk '{print $1}' log.txt", "go list ./...", "go env GOPATH", "npm ls --depth=0", "node --version",
    "echo \"it's fine\"", "jq '.name' package.json", "test -f go.mod && echo yes || echo no",
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
  ]) assert.equal(plainRead(line), false, JSON.stringify(line));
  assert.equal(plainRead(undefined), false);
});

test("the line is split the way a shell would split it", () => {
  assert.deepEqual(lex("grep -n 'a b' \"c d\" e\\ f | wc -l"), [["grep", "-n", "a b", "c d", "e f"], ["wc", "-l"]]);
  assert.deepEqual(lex("ls 2>/dev/null; pwd"), [["ls"], ["pwd"]]);
  assert.deepEqual(lex("a && b || c"), [["a"], ["b"], ["c"]]);
  assert.equal(lex("a > b"), null);
});
