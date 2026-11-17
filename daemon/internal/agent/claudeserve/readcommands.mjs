// Which shell commands only read. A thread member that has not changed
// anything yet works in its repo's own folder, where a person or another
// session may be working too, so there it may run only what plainly reads
// (readonly.mjs). plainRead answers for one command line.
//
// The answer leans to "no": a command wrongly refused costs the member a
// move into its own worktree, where it runs anything; one wrongly let
// through writes into somebody else's folder. So the line is read the way a
// shell would (quotes, pipes, redirects), anything this file cannot follow
// is a no, and each command is on a short list with the flags that would
// make it write taken out. Pure: no I/O.

// lex splits a command line into commands (lists of words), or returns null
// when it holds something that can write or run more than is written:
// a redirect into a file, a substitution, a subshell, a background job, a
// here-document.
export function lex(line) {
  const cmds = [[]];
  let word = null; // the word being read, null between words
  const end = () => { if (word !== null) cmds[cmds.length - 1].push(word); word = null; };
  const next = () => { end(); cmds.push([]); };
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    const two = line.slice(i, i + 2);
    if (c === "'") {
      const close = line.indexOf("'", i + 1);
      if (close < 0) return null;
      word = (word ?? "") + line.slice(i + 1, close);
      i = close;
    } else if (c === '"') {
      const got = doubleQuoted(line, i + 1);
      if (!got) return null;
      word = (word ?? "") + got.text;
      i = got.end;
    } else if (c === "\\") {
      if (i + 1 >= line.length) return null;
      word = (word ?? "") + line[++i];
    } else if (c === " " || c === "\t") end();
    else if (c === "\n" || c === ";") next();
    else if (two === "&&" || two === "||") { next(); i++; }
    else if (c === "|") next();
    else if (c === ">" || two === "&>") {
      const skip = harmlessRedirect(line, i, word);
      if (skip < 0) return null;
      if (/^\d+$/.test(word ?? "")) word = null; // the "2" of 2>/dev/null
      end();
      i = skip;
    } else if ("`()<&".includes(c) || two === "$(") return null;
    else word = (word ?? "") + c;
  }
  end();
  return cmds.filter((c) => c.length);
}

// doubleQuoted reads a double-quoted stretch from start (just past the
// opening quote): its text and where its closing quote is, or null when it
// never closes or runs a command inside it.
function doubleQuoted(line, start) {
  let text = "";
  for (let i = start; i < line.length; i++) {
    const c = line[i];
    if (c === '"') return { text, end: i };
    if (c === "`" || line.slice(i, i + 2) === "$(") return null;
    if (c === "\\" && i + 1 < line.length) text += line[++i];
    else text += c;
  }
  return null;
}

// harmlessRedirect takes a redirect that starts at i and returns the index
// of its last character when it writes nowhere (into /dev/null, or one
// stream into another: 2>&1), -1 otherwise.
function harmlessRedirect(line, i, word) {
  const m = /^(&?>>?)\s*(&\d|\/dev\/null)(?=[\s;|&)]|$)/.exec(line.slice(i));
  if (!m) return -1;
  if (word !== null && word !== "" && !/^\d+$/.test(word)) return -1; // glued to a word: not a shape this file follows
  return i + m[0].length - 1;
}

const has = (args, ...flags) => args.some((a) => flags.some((f) => a === f || a.startsWith(f + "=")));
const positional = (args) => args.filter((a) => !a.startsWith("-"));
const anyStarts = (args, ...prefixes) => args.some((a) => prefixes.some((p) => a.startsWith(p)));

// always are commands that read whatever their arguments.
const always = new Set([
  "ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "rg", "ag", "tree", "stat", "file", "du", "df", "pwd", "cd",
  "echo", "printf", "date", "which", "type", "whoami", "id", "uname", "hostname", "basename", "dirname", "realpath", "readlink",
  "cut", "tr", "column", "nl", "tac", "rev", "diff", "cmp", "comm", "seq", "true", "false", "test", "[", "sleep", "jq", "printenv",
  "md5sum", "sha1sum", "sha256sum", "strings", "od", "hexdump", "less", "more", "ps", "pgrep", "uptime", "free", "nproc", "lsblk",
]);

// gitReads are git's subcommands that read, each with the test its
// arguments must pass (true for any).
const gitAny = () => true;
const noCreate = (args) => positional(args).length === 0 && !anyStarts(args, "-d", "-D", "-m", "-M", "-c", "-C", "-u", "-f", "--delete", "--move", "--copy", "--set-upstream", "--unset-upstream", "--force", "--edit-description");
const gitReads = {
  status: gitAny, log: gitAny, diff: gitAny, show: gitAny, "rev-parse": gitAny, "ls-files": gitAny, "ls-tree": gitAny, blame: gitAny,
  grep: (args) => !anyStarts(args, "-O", "--open-files-in-pager"), describe: gitAny, "cat-file": gitAny, shortlog: gitAny,
  "merge-base": gitAny, "rev-list": gitAny, "name-rev": gitAny, "for-each-ref": gitAny, "show-ref": gitAny, whatchanged: gitAny,
  "count-objects": gitAny, "check-ignore": gitAny, "check-attr": gitAny, "show-branch": gitAny, cherry: gitAny, "range-diff": gitAny,
  "diff-tree": gitAny, version: gitAny,
  reflog: (args) => !args.length || args[0] === "show",
  worktree: (args) => args[0] === "list",
  stash: (args) => args[0] === "list" || args[0] === "show",
  remote: (args) => !args.length || ["-v", "--verbose", "show", "get-url"].includes(args[0]),
  config: (args) => has(args, "--get", "--get-all", "--get-regexp", "--list", "-l"),
  branch: noCreate,
  tag: (args) => positional(args).length === 0 && !anyStarts(args, "-d", "-a", "-s", "-f", "-m", "-F", "-u", "--delete", "--force"),
  "symbolic-ref": (args) => positional(args).length <= 1 && !has(args, "-d", "--delete", "-m"),
};

// git reads when its subcommand does, no option makes it run or write
// something (-c can name a program to run, --output a file to write), and
// the options before the subcommand are ones that only say where to look.
function git(args) {
  let i = 0;
  while (i < args.length && args[i].startsWith("-")) {
    if (args[i] === "--no-pager") i += 1;
    else if (args[i] === "-C" || args[i] === "--git-dir" || args[i] === "--work-tree") i += 2;
    else return false;
  }
  const rule = gitReads[args[i]];
  const rest = args.slice(i + 1);
  return !!rule && !anyStarts(rest, "--output", "--ext-diff") && rule(rest);
}

// printsOnly is a sed script that only prints lines: "10,20p", "$p", "5q".
const printsOnly = /^[0-9,$~+ ]*[pq=]?(;[0-9,$~+ ]*[pq=]?)*$/;

function sed(args) {
  if (!has(args, "-n", "--quiet", "--silent") || anyStarts(args, "-i", "--in-place", "-f", "--file", "-s", "-e", "--expression", "-E", "-r", "-z")) return false;
  const script = positional(args)[0];
  return typeof script === "string" && printsOnly.test(script);
}

const versionOnly = (args) => args.length === 1 && ["--version", "-v", "-V", "version"].includes(args[0]);

// rules are the commands that read only with the right arguments.
const rules = {
  git,
  sed,
  sort: (args) => !anyStarts(args, "-o", "--output"),
  uniq: (args) => positional(args).length <= 1,
  find: (args) => !anyStarts(args, "-delete", "-exec", "-ok", "-fprint", "-fls"),
  fd: (args) => !anyStarts(args, "-x", "-X", "--exec"),
  awk: (args) => !args.some((a) => /[>|]|system|getline/.test(a)) && !anyStarts(args, "-f", "--file", "-i", "--include"),
  yq: (args) => !anyStarts(args, "-i", "--inplace"),
  env: (args) => args.length === 0,
  go: (args) => ["version", "list", "doc"].includes(args[0]) || (args[0] === "env" && !has(args, "-w", "-u")),
  npm: (args) => ["ls", "list", "view", "root", "prefix", "outdated"].includes(args[0]),
  node: versionOnly, python: versionOnly, python3: versionOnly, claude: versionOnly,
};

// reads says whether one command (its words) only reads. Variables set in
// front of it (FOO=1 cmd) are let through: with substitutions already
// refused they are plain text.
function reads(words) {
  let i = 0;
  while (i < words.length && /^[A-Za-z_][A-Za-z0-9_]*=/.test(words[i])) i++;
  if (i === words.length) return true; // only assignments
  const name = words[i];
  const args = words.slice(i + 1);
  if (always.has(name)) return true;
  return Object.hasOwn(rules, name) && rules[name](args);
}

// plainRead reports whether every command on the line only reads.
export function plainRead(line) {
  if (typeof line !== "string" || !line.trim()) return false;
  const cmds = lex(line);
  return !!cmds && cmds.length > 0 && cmds.every(reads);
}
