#!/usr/bin/env python3
"""PreToolUse(Bash) guard: main/master are owner-only (KIN-A-20); the agent may merge PRs into dev (KIN-12).

Reads the hook JSON on stdin. Exit 0 = allow, exit 2 = block (stderr goes back to Claude).
The command is tokenized with shlex (quotes removed) and checked segment by segment, so quoting, attached
flags and global options cannot hide a protected action. Anything that cannot be analysed reliably and
touches merges/pushes is blocked (fail closed).
"""
import json
import re
import shlex
import subprocess
import sys

PROTECTED = {"main", "master"}
SEPARATORS = {";", "&&", "||", "|", "&", "(", ")", "\n"}
# Top-level gh commands; anything else may be a user alias (e.g. `gh pm` → `pr merge`).
GH_COMMANDS = {
    "pr", "api", "issue", "run", "repo", "auth", "workflow", "release", "search", "browse", "label",
    "status", "cache", "gist", "project", "ruleset", "secret", "variable", "codespace", "org", "config",
    "help", "version", "completion", "attestation", "ssh-key", "gpg-key", "extension",
}
MERGE_FLAGS = {"--merge", "-m", "--squash", "-s", "--rebase", "-r", "--delete-branch", "-d", "--auto"}
RISKY_WORDS = re.compile(r"\b(merge|push|mergePullRequest)\b|pr\s+(create|edit)|git/refs", re.I)


class Blocked(Exception):
    pass


def block(reason: str):
    raise Blocked(reason)


def segments(cmd: str):
    lex = shlex.shlex(cmd.replace("\n", " ; "), posix=True, punctuation_chars=";&|()")
    lex.whitespace_split = True
    seg = []
    for tok in lex:
        if tok in SEPARATORS or set(tok) <= set(";&|()"):
            if seg:
                yield seg
            seg = []
        else:
            seg.append(tok)
    if seg:
        yield seg


def strip_env(seg):
    """Drops `env` and VAR=value prefixes; refuses GH_*/GIT_* overrides for gh/git (they redirect repos)."""
    i = 0
    if seg and seg[0] == "env":
        i = 1
    assigned = []
    while i < len(seg) and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", seg[i]):
        assigned.append(seg[i].split("=", 1)[0])
        i += 1
    rest = seg[i:]
    if rest and rest[0] in ("gh", "git") and any(v.startswith(("GH_", "GIT_")) for v in assigned):
        block(f"{rest[0]} with {', '.join(assigned)} override")
    return rest


def flag_value(args, short, long):
    """Values of -X v / -Xv / --long v / --long=v."""
    out = []
    for i, a in enumerate(args):
        if a in (short, long) and i + 1 < len(args):
            out.append(args[i + 1])
        elif long and a.startswith(long + "="):
            out.append(a.split("=", 1)[1])
        elif short and a.startswith(short) and len(a) > len(short) and not a.startswith("--"):
            out.append(a[len(short):])
    return out


def pr_base(number: str) -> str:
    try:
        r = subprocess.run(["gh", "pr", "view", number, "--json", "baseRefName", "--jq", ".baseRefName"],
                           capture_output=True, text=True, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return r.stdout.strip() if r.returncode == 0 else ""


def check_gh(args, state):
    if not args:
        return
    if args[0] not in GH_COMMANDS:
        block(f"unknown gh command '{args[0]}' (global flags before the command, or a possible alias)")
    cmd, sub, rest = args[0], args[1] if len(args) > 1 else "", args[2:]
    if cmd == "api":
        method = (flag_value(args[1:], "-X", "--method") or [""])[-1].upper()
        writes = any(a in ("-f", "-F", "--field", "--raw-field", "--input") or
                     re.match(r"^(-f|-F).|^--(raw-)?field=|^--input=", a) for a in args[1:])
        if not method:
            method = "POST" if writes else "GET"
        if method != "GET" or any("mutation" in a for a in args):
            block(f"gh api {method} (raw API writes are owner-only)")
        return
    if cmd != "pr":
        return
    if sub == "merge":
        if state.get("merged"):
            block("one merge per command")
        state["merged"] = True
        if not rest or not rest[0].isdigit():
            block("gh pr merge needs a bare PR number first")
        unknown = [a for a in rest[1:] if a not in MERGE_FLAGS]
        if unknown:
            block(f"gh pr merge flags not allowed: {' '.join(unknown)}")
        base = pr_base(rest[0])
        if base != "dev":
            block(f"merge of PR #{rest[0]} whose base is '{base or 'unknown'}' (only dev is allowed)")
    elif sub == "create":
        bases = flag_value(rest, "-B", "--base")
        if not bases:
            block("gh pr create without --base (defaults to main)")
        if set(bases) & PROTECTED:
            block("PR into main/master")
    elif sub == "edit":
        if set(flag_value(rest, "-B", "--base")) & PROTECTED:
            block("retarget PR to main/master")
    elif sub not in ("view", "list", "checks", "diff", "status", "comment", "close", "reopen", "ready", "checkout", "review"):
        if any("merge" in a for a in args):
            block(f"gh pr {sub} with merge arguments")


GIT_GLOBAL_WITH_VALUE = {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path"}


def check_git(args, state):
    i = 0
    while i < len(args) and args[i].startswith("-"):
        i += 2 if args[i] in GIT_GLOBAL_WITH_VALUE else 1
    if i >= len(args):
        return
    sub, rest = args[i], args[i + 1:]
    if sub == "push":
        if any(a in ("--all", "--mirror") for a in rest):
            block("git push --all/--mirror")
        for a in (a for a in rest if not a.startswith("-")):
            dst = a.lstrip("+").split(":")[-1].removeprefix("refs/heads/")
            if dst in PROTECTED:
                block("push to main/master")
        if current_branch() in PROTECTED:
            block("push from a checked-out main/master")
    elif sub in ("checkout", "switch"):
        state["on_protected"] = any(a in PROTECTED for a in rest)
    elif sub == "merge" and state.get("on_protected"):
        block("merge into main/master")


def current_branch() -> str:
    try:
        r = subprocess.run(["git", "branch", "--show-current"], capture_output=True, text=True, timeout=5)
        return r.stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        return ""


def check(cmd: str, depth: int = 0):
    if depth > 3:
        block("nested shells too deep to analyse")
    if ("$(" in cmd or "`" in cmd or "<(" in cmd) and RISKY_WORDS.search(cmd):
        block("command substitution around merge/push/PR commands cannot be checked")
    try:
        segs = list(segments(cmd))
    except ValueError:  # unbalanced quotes
        if RISKY_WORDS.search(cmd):
            block("unparsable command mentioning merge/push")
        return
    state = {}
    for seg in segs:
        args = strip_env(seg)
        if not args:
            continue
        prog = args[0].rsplit("/", 1)[-1]
        if prog in ("bash", "sh", "zsh", "dash") and "-c" in args:
            idx = args.index("-c")
            if idx + 1 < len(args):
                check(args[idx + 1], depth + 1)
        elif prog == "eval":
            check(" ".join(args[1:]), depth + 1)
        elif prog in ("xargs", "nohup", "time", "timeout", "nice", "sudo", "command", "exec"):
            if RISKY_WORDS.search(" ".join(args)) or any(a in ("gh", "git") for a in args[1:]):
                block(f"gh/git wrapped in {prog} cannot be checked")
        elif prog == "gh":
            check_gh(args[1:], state)
        elif prog == "git":
            check_git(args[1:], state)


def main() -> int:
    try:
        cmd = (json.load(sys.stdin).get("tool_input") or {}).get("command") or ""
    except (ValueError, AttributeError):
        return 0
    try:
        check(cmd)
    except Blocked as e:
        print(f"BLOCKED by .claude/hooks/guard-git.sh: {e}. main/master are owner-only; target dev instead.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
