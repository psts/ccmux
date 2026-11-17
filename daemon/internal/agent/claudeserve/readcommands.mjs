// Which shell commands only read. A thread member that has not changed
// anything yet works in its repo's own folder, where a person or another
// session may be working too, so there it may run only what plainly reads
// (readonly.mjs). plainRead answers for one command line.
//
// The answer leans to "no": a command wrongly refused costs the member a
// move into its own worktree, where it runs anything; one wrongly let
// through writes into somebody else's folder. So the line is read the way a
// shell would (quotes, pipes, redirects), anything this file cannot follow
// is a no, and each command is on a short list. The shell's own expansions
// are refused outright: a variable, a brace list or a word that starts with
// a wildcard can each turn into a flag this file never saw ("$X" into
// "-delete", "{-delete,}" too, "*" into a file named "-delete"). Commands
// that have a flag which writes or runs something take only the flags
// listed for them, not all but a few. Pure: no I/O.

// lex splits a command line into commands (lists of words), or returns null
// when it holds something that can write or run more than is written:
// a redirect into a file, a substitution, a subshell, a background job, a
// here-document.
export function lex(line) {
  const sc = { line, i: 0, cmds: [[]], word: null };
  for (; sc.i < line.length; sc.i++) {
    const read = readers[line[sc.i]] || plain;
    if (!read(sc)) return null;
  }
  endWord(sc);
  return sc.cmds.filter((c) => c.length);
}

// The scanner's state is sc: the line, where it is, the commands so far and
// the word being read (null between words). Each reader takes the character
// at sc.i, may move sc.i on to the last character it used, and returns
// false when the line is not one this file follows.
const endWord = (sc) => { if (sc.word !== null) sc.cmds[sc.cmds.length - 1].push(sc.word); sc.word = null; return true; };
const nextCommand = (sc) => { endWord(sc); sc.cmds.push([]); return true; };
const add = (sc, text) => { sc.word = (sc.word ?? "") + text; return true; };
const refuse = () => false;
// plain is any other character. A wildcard is refused when nothing but
// empty quotes ('' and "" add no text) or a "-" comes before it in its
// word: what it matches is not on the line, and a file named "-delete"
// would be a flag. One after other text ("src/*.go") only matches names
// that start with that text.
const plain = (sc) => ("*?[".includes(sc.line[sc.i]) && (!sc.word || sc.word.startsWith("-")) ? false : add(sc, sc.line[sc.i]));

function singleQuote(sc) {
  const close = sc.line.indexOf("'", sc.i + 1);
  if (close < 0) return false;
  add(sc, sc.line.slice(sc.i + 1, close));
  sc.i = close;
  return true;
}

function doubleQuote(sc) {
  const got = doubleQuoted(sc.line, sc.i + 1);
  if (!got) return false;
  add(sc, got.text);
  sc.i = got.end;
  return true;
}

// escape is a backslash: the next character as itself, or, before a
// newline, nothing at all (the shell joins the two lines, so "-del\<newline>ete"
// is "-delete").
function escape(sc) {
  if (sc.i + 1 >= sc.line.length) return false;
  if (sc.line[sc.i + 1] === "\n") { sc.i++; return true; }
  return add(sc, sc.line[++sc.i]);
}

// pipe is | or ||: either way the next command starts.
function pipe(sc) {
  if (sc.line[sc.i + 1] === "|") sc.i++;
  return nextCommand(sc);
}

// ampersand is && (the next command), &> (a redirect) or a background job.
function ampersand(sc) {
  const next = sc.line[sc.i + 1];
  if (next === "&") { sc.i++; return nextCommand(sc); }
  return next === ">" ? redirect(sc) : false;
}

function redirect(sc) {
  const last = harmlessRedirect(sc.line, sc.i, sc.word);
  if (last < 0) return false;
  if (/^\d+$/.test(sc.word ?? "")) sc.word = null; // the "2" of 2>/dev/null
  endWord(sc);
  sc.i = last;
  return true;
}

// "$" (a variable, $'...', a substitution) and an unquoted brace list are
// the shell putting in text this file cannot see, so both are refused.
const readers = {
  "'": singleQuote, '"': doubleQuote, "\\": escape, " ": endWord, "\t": endWord, "\n": nextCommand, ";": nextCommand,
  "|": pipe, "&": ampersand, ">": redirect, "$": refuse, "`": refuse, "(": refuse, ")": refuse, "<": refuse,
  "{": refuse, "}": refuse,
};

// doubleQuoted reads a double-quoted stretch from start (just past the
// opening quote): its text and where its closing quote is, or null when it
// never closes or runs a command inside it.
function doubleQuoted(line, start) {
  let text = "";
  for (let i = start; i < line.length; i++) {
    const c = line[i];
    if (c === '"') return { text, end: i };
    if (c === "`" || c === "$") return null; // a variable or a substitution: text this file cannot see
    if (c === "\\" && line[i + 1] === "\n") i++; // joined, as outside quotes
    else if (c === "\\" && i + 1 < line.length) text += line[++i];
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
// short reports a one-letter flag among letters, alone or in a cluster with
// others ("-uo"), or with its value glued on ("-oout.txt").
const short = (args, letters) => args.some((a) => a.startsWith("-") && !a.startsWith("--") && [...letters].some((l) => a.slice(1).includes(l)));

// always are commands that read whatever their arguments. A pager is not
// among them: it can write a log file and run a preprocessor, and a session
// with no terminal has no use for one.
const always = new Set([
  "ls", "cat", "head", "tail", "wc", "grep", "egrep", "fgrep", "stat", "du", "df", "pwd", "cd",
  "echo", "printf", "date", "which", "type", "whoami", "id", "uname", "hostname", "basename", "dirname", "realpath", "readlink",
  "cut", "tr", "column", "nl", "tac", "rev", "diff", "cmp", "comm", "seq", "true", "false", "test", "[", "sleep", "jq", "printenv",
  "md5sum", "sha1sum", "sha256sum", "strings", "od", "hexdump", "ps", "pgrep", "uptime", "free", "nproc", "lsblk",
]);

// gitReads are git's subcommands that read, each with the test its
// arguments must pass (true for any).
const gitAny = () => true;
const noCreate = (args) => positional(args).length === 0 && !anyStarts(args, "-d", "-D", "-m", "-M", "-c", "-C", "-u", "-f", "--delete", "--move", "--copy", "--set-upstream", "--unset-upstream", "--force", "--edit-description");
const gitReads = {
  status: gitAny, log: gitAny, diff: gitAny, show: gitAny, "rev-parse": gitAny, "ls-files": gitAny, "ls-tree": gitAny, blame: gitAny,
  grep: (args) => !short(args, "O") && !anyStarts(args, "--op"), // --open-files-in-pager runs a program, cut short or not describe: gitAny, "cat-file": gitAny, shortlog: gitAny,
  "merge-base": gitAny, "rev-list": gitAny, "name-rev": gitAny, "for-each-ref": gitAny, "show-ref": gitAny, whatchanged: gitAny,
  "count-objects": gitAny, "check-ignore": gitAny, "check-attr": gitAny, "show-branch": gitAny, cherry: gitAny, "range-diff": gitAny,
  "diff-tree": gitAny, version: gitAny,
  reflog: (args) => !args.length || args[0] === "show",
  worktree: (args) => args[0] === "list",
  stash: (args) => args[0] === "list" || args[0] === "show",
  remote: (args) => {
    const rest = args[0] === "-v" || args[0] === "--verbose" ? args.slice(1) : args; // "git remote -v add" adds
    return !rest.length || rest[0] === "show" || rest[0] === "get-url";
  },
  config: (args) => has(args, "--get", "--get-all", "--get-regexp", "--list", "-l") && !anyStarts(args, "--add", "--unset", "--replace-all", "--rename-section", "--remove-section", "--edit", "-e"),
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
  // "--ou" and not "--output": git takes a long option cut short ("--outp=x").
  return !!rule && !anyStarts(rest, "--ou", "--ext") && rule(rest);
}

// printsOnly is a sed script that only prints lines: "10,20p", "$p", "5q".
const printsOnly = /^[0-9,$~+ ]*[pq=]?(;[0-9,$~+ ]*[pq=]?)*$/;

// sed reads with -n and a script that only prints. No other flag: GNU sed
// takes a long option cut short, so "--i" is --in-place.
function sed(args) {
  if (!has(args, "-n", "--quiet", "--silent") || !onlyFlags(args, sedFlags)) return false;
  const script = positional(args)[0];
  return typeof script === "string" && printsOnly.test(script);
}

// onlyFlags reports whether every flag in args is one spec lists: a cluster
// of its letters ("-rn"), a letter that takes a value, glued on or the next
// word ("-k2", "-t :"), or one of its long flags ("--key=2", "--key 2").
// Anything else, a long flag cut short among them, is a no.
function onlyFlags(args, spec) {
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a === "--") return true;
    if (!a.startsWith("-") || a === "-") continue;
    const step = a.startsWith("--") ? longFlag(a, args[i + 1], spec) : shortFlags(a, spec);
    if (step < 0) return false;
    i += step;
  }
  return true;
}

// longFlag returns how many more words a long flag uses (0 or 1), or -1.
function longFlag(a, next, spec) {
  const [name, value] = a.split("=", 2);
  if ((spec.long || []).includes(name) && value === undefined) return 0;
  if (!(spec.longValued || []).includes(name)) return -1;
  return value === undefined ? (next === undefined ? -1 : 1) : 0;
}

function shortFlags(a, spec) {
  for (let j = 1; j < a.length; j++) {
    if ((spec.letters || "").includes(a[j])) continue;
    if (!(spec.valued || "").includes(a[j])) return -1;
    return j === a.length - 1 ? 1 : 0; // its value is the next word, or the rest of this one
  }
  return 0;
}

const sedFlags = { letters: "n", long: ["--quiet", "--silent"] };

// sort without -o (writes), -T/-S (temporary files, and with them
// --compress-program, which runs a program).
const sortFlags = {
  letters: "bcdfghiMnRrsuVz", valued: "kt",
  long: ["--ignore-leading-blanks", "--check", "--dictionary-order", "--ignore-case", "--general-numeric-sort", "--human-numeric-sort",
    "--ignore-nonprinting", "--month-sort", "--numeric-sort", "--random-sort", "--reverse", "--stable", "--unique", "--version-sort", "--zero-terminated"],
  longValued: ["--key", "--field-separator"],
};

// tree without -o (writes) and -R (with -H, writes a page in every folder).
const treeFlags = {
  letters: "adflsFpughDiCnqNQrtvUJ", valued: "LIP",
  long: ["--noreport", "--dirsfirst", "--du", "--prune", "--gitignore", "--matchdirs", "--ignore-case", "--version", "--help"],
  longValued: ["--charset", "--filelimit", "--sort"],
};

// findTests are find's tests and actions that only look and print. -delete,
// -exec, -ok and the -f... ones that write a file are not among them.
const findTests = new Set([
  "-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-regex", "-iregex", "-regextype", "-type", "-xtype",
  "-maxdepth", "-mindepth", "-newer", "-anewer", "-cnewer", "-size", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin",
  "-empty", "-print", "-print0", "-printf", "-ls", "-not", "-o", "-a", "-and", "-or", "-perm", "-user", "-group", "-uid", "-gid",
  "-nouser", "-nogroup", "-links", "-lname", "-ilname", "-readable", "-writable", "-executable", "-prune", "-depth", "-follow",
  "-L", "-H", "-P", "-true", "-false", "-samefile", "-inum", "-quit", "-mount", "-xdev",
]);

// find reads when every word that looks like a flag is one of findTests,
// or a number a test takes ("-mtime -1", "-size -10k").
const find = (args) => args.every((a) => !a.startsWith("-") || findTests.has(a) || /^-\d+[a-zA-Z]?$/.test(a));

const versionOnly = (args) => args.length === 1 && ["--version", "-v", "-V", "version"].includes(args[0]);

// rules are the commands that read only with the right arguments.
const rules = {
  git,
  sed,
  sort: (args) => onlyFlags(args, sortFlags),
  tree: (args) => onlyFlags(args, treeFlags),
  file: (args) => !short(args, "C") && !anyStarts(args, "--c"), // --compile, cut short or not
  uniq: (args) => positional(args).length <= 1,
  find,
  fd: (args) => !short(args, "xX") && !anyStarts(args, "--exec"),
  rg: (args) => !anyStarts(args, "--pre", "--hostname-bin"),
  ag: (args) => !anyStarts(args, "--pager"),
  yq: (args) => !short(args, "i") && !anyStarts(args, "--inplace"),
  env: (args) => args.length === 0,
  go: (args) => ["version", "list", "doc"].includes(args[0]) || (args[0] === "env" && !has(args, "-w", "-u")),
  npm: (args) => ["ls", "list", "view", "root", "prefix", "outdated"].includes(args[0]),
  node: versionOnly, python: versionOnly, python3: versionOnly, claude: versionOnly,
};

// harmlessVar is a variable that only changes how a command prints: its
// language, its time zone, its colours. Any other one set in front of a
// command is refused, since many name a program to run (GIT_EXTERNAL_DIFF,
// LESSOPEN, PAGER).
const harmlessVar = /^(LC_[A-Z]+|LANG|LANGUAGE|TZ|NO_COLOR|TERM|COLUMNS|LINES)=/;

// reads says whether one command (its words) only reads.
function reads(words) {
  let i = 0;
  while (i < words.length && /^[A-Za-z_][A-Za-z0-9_]*=/.test(words[i])) {
    if (!harmlessVar.test(words[i])) return false;
    i++;
  }
  if (i === words.length) return false; // only assignments: nothing to read, and the shell may use them later
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
