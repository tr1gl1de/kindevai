#!/usr/bin/env python3
"""PreToolUse(Bash) guard: main/master are owner-only (KIN-A-20); the agent may merge PRs into dev (KIN-12).

Reads the hook JSON on stdin. Exit 0 = allow, exit 2 = block (stderr goes back to Claude).

This is defence in depth, not the security boundary: a text check cannot see every indirection, so the owner's
GitHub rulesets on main/master and v* tags remain the real protection. The guard tokenizes each command with
shlex (quotes removed, heredoc bodies dropped, substitutions analysed recursively) and fails closed:
  * a PR merge must be the only command: `gh pr merge <number> [method/subject/body flags]`, and the PR's base,
    looked up from the session's working directory without GH_*/GIT_* redirection, must be dev;
  * raw `gh api` writes and GraphQL mutations, unknown gh commands (aliases) and alias creation are blocked;
  * pushes and PRs that target main/master, `--all`/`--mirror`, variable refspecs and push-related config
    overrides are blocked.
"""
import json
import os
import re
import shlex
import subprocess
import sys

PROTECTED = {"main", "master"}
PUNCT = ";&|()"
# Top-level gh commands; anything else may be a user alias (e.g. `gh pm` → `pr merge`).
GH_COMMANDS = {
    "pr", "api", "issue", "run", "repo", "auth", "workflow", "release", "search", "browse", "label",
    "status", "cache", "gist", "project", "ruleset", "secret", "variable", "codespace", "org", "config",
    "help", "version", "completion", "attestation", "ssh-key", "gpg-key", "extension",
}
GH_PR_READ_ONLY = {"view", "list", "checks", "diff", "status", "comment", "close", "reopen", "ready", "checkout",
                   "review", "create", "edit"}
MERGE_FLAGS = {"--merge", "-m", "--squash", "-s", "--rebase", "-r", "--delete-branch", "-d", "--auto"}
MERGE_VALUE_FLAGS = {"--subject", "-t", "--body", "-b"}
REDIRECTING_ENV = ("GH_", "GIT_")
GIT_GLOBAL_WITH_VALUE = {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path"}
GIT_RISKY_CONFIG = re.compile(r"^(alias|remote|url|push|branch|include|includeif)\.", re.I)

HEREDOC = re.compile(r"<<-?[ \t]*(['\"]?)(\w+)\1([^\n]*)\n.*?\n[ \t]*\2[ \t]*(?=\n|$)", re.S)
FD_DUP = re.compile(r"\d*[<>]&\d*-?|&>>?")
REDIRECT = re.compile(r"^\d*(<<?|>>?)(.*)$")


class Blocked(Exception):
    pass


def block(reason: str):
    raise Blocked(reason)


def substitutions(cmd: str):
    """Contents of $(...) (balanced parentheses) and `...` anywhere in the text, including inside quotes."""
    out = re.findall(r"`([^`]*)`", cmd)
    i = cmd.find("$(")
    while i != -1:
        depth, j = 0, i + 1
        while j < len(cmd):
            depth += {"(": 1, ")": -1}.get(cmd[j], 0)
            if depth == 0:
                break
            j += 1
        out.append(cmd[i + 2:j])
        i = cmd.find("$(", i + 2)
    return out


def segments(cmd: str):
    lex = shlex.shlex(FD_DUP.sub(" ", cmd).replace("\n", " ; "), posix=True, punctuation_chars=PUNCT)
    lex.whitespace_split = True
    seg, skip = [], False
    for tok in lex:
        if skip:
            skip = False
            continue
        if set(tok) <= set(PUNCT):
            if seg:
                yield seg
            seg = []
            continue
        m = REDIRECT.match(tok)
        if m:  # redirections are not arguments; `> file` also consumes the next token
            skip = m.group(2) == ""
            continue
        seg.append(tok)
    if seg:
        yield seg


def strip_env(seg):
    """Drops `env` and VAR=value prefixes; refuses GH_*/GIT_* overrides (they redirect gh/git to other repos)."""
    i = 1 if seg and seg[0] == "env" else 0
    while i < len(seg) and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", seg[i]):
        if seg[i].startswith(REDIRECTING_ENV):
            block(f"{seg[i].split('=', 1)[0]} override")
        i += 1
    return seg[i:]


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


def clean_env():
    return {k: v for k, v in os.environ.items() if not k.startswith(REDIRECTING_ENV)}


def run(args, cwd, timeout):
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=timeout, cwd=cwd or None, env=clean_env())
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return r.stdout.strip() if r.returncode == 0 else ""


def check_gh(args, ctx):
    if not args:
        return
    if args[0] not in GH_COMMANDS:
        block(f"unknown gh command '{args[0]}' (global flags before the command, or a possible alias)")
    cmd, sub, rest = args[0], args[1] if len(args) > 1 else "", args[2:]
    if cmd == "api":
        method = (flag_value(args[1:], "-X", "--method") or [""])[-1].upper()
        writes = any(a in ("-f", "-F", "--field", "--raw-field", "--input") or
                     re.match(r"^(-f|-F).|^--(raw-)?field=|^--input=", a) for a in args[1:])
        method = method or ("POST" if writes else "GET")
        if method != "GET" or any("mutation" in a.lower() for a in args):
            block(f"gh api {method} (raw API writes are owner-only)")
        return
    if cmd == "alias" or (cmd == "config" and "alias" in args):
        block("gh aliases")
    if cmd != "pr":
        return
    if sub == "merge":
        check_merge(rest, ctx)
    elif sub == "create":
        bases = flag_value(rest, "-B", "--base")
        if not bases:
            block("gh pr create without --base (defaults to main)")
        if set(bases) & PROTECTED:
            block("PR into main/master")
    elif sub == "edit":
        if set(flag_value(rest, "-B", "--base")) & PROTECTED:
            block("retarget PR to main/master")
    elif sub not in GH_PR_READ_ONLY:
        block(f"gh pr {sub or '(none)'} is not on the allowlist")


def check_merge(rest, ctx):
    if ctx["segments"] != 1 or ctx["substituted"]:
        block("a PR merge must be the only command (no cd, export or chaining around it)")
    if any(os.environ.get(k) for k in ("GH_REPO", "GH_HOST", "GIT_DIR", "GIT_WORK_TREE")):
        block("GH_REPO/GH_HOST/GIT_DIR is set in the environment")
    if not rest or not rest[0].isdigit():
        block("gh pr merge needs a bare PR number first")
    i = 1
    while i < len(rest):
        a = rest[i]
        if a in MERGE_VALUE_FLAGS:
            i += 2
            continue
        if a not in MERGE_FLAGS and not a.startswith(("--subject=", "--body=")):
            block(f"gh pr merge flag not allowed: {a}")
        i += 1
    base = run(["gh", "pr", "view", rest[0], "--json", "baseRefName", "--jq", ".baseRefName"], ctx["cwd"], 20)
    if base != "dev":
        block(f"merge of PR #{rest[0]} whose base is '{base or 'unknown'}' (only dev is allowed)")


def check_git(args, ctx):
    i, globals_ = 0, []
    while i < len(args) and args[i].startswith("-"):
        n = 2 if args[i] in GIT_GLOBAL_WITH_VALUE else 1
        globals_ += args[i:i + n]
        i += n
    for v in flag_value(globals_, "-c", ""):
        if GIT_RISKY_CONFIG.match(v):
            block(f"git -c {v.split('=', 1)[0]}")
    if i >= len(args):
        return
    sub, rest = args[i], args[i + 1:]
    if sub == "push":
        if any(a in ("--all", "--mirror") or a.startswith("--repo") for a in rest):
            block("git push --all/--mirror/--repo")
        for a in rest:
            if any(c in a for c in "${}`"):
                block("git push with a variable argument")
            if a.startswith("-"):
                continue
            dst = a.lstrip("+").split(":")[-1].removeprefix("refs/heads/")
            if dst in PROTECTED:
                block("push to main/master")
        if run(["git", *globals_, "branch", "--show-current"], ctx["cwd"], 5) in PROTECTED:
            block("push from a checked-out main/master")
    elif sub == "config":
        reading = any(a in ("--get", "--get-all", "--get-regexp", "--list", "-l") for a in rest)
        if not reading and any(GIT_RISKY_CONFIG.match(a) for a in rest):
            block("git config of alias/remote/url/push settings")
    elif sub in ("checkout", "switch"):
        ctx["on_protected"] = any(a in PROTECTED for a in rest)
    elif sub == "merge" and ctx.get("on_protected"):
        block("merge into main/master")


def check(cmd: str, cwd: str, depth: int = 0, substituted: bool = False):
    if depth > 3:
        block("nested shells too deep to analyse")
    cmd = HEREDOC.sub(lambda m: "<<HEREDOC" + m.group(3), cmd)
    for inner in substitutions(cmd):
        check(inner, cwd, depth + 1, substituted=True)
    try:
        segs = [s for s in (strip_env(s) for s in segments(cmd)) if s]
    except ValueError:  # unbalanced quotes
        if re.search(r"\b(gh|git)\b", cmd):
            block("unparsable gh/git command")
        return
    ctx = {"cwd": cwd, "segments": len(segs), "substituted": substituted}
    for args in segs:
        prog = args[0].rsplit("/", 1)[-1]
        if prog in ("bash", "sh", "zsh", "dash") and "-c" in args:
            idx = args.index("-c")
            if idx + 1 < len(args):
                check(args[idx + 1], cwd, depth + 1, substituted)
        elif prog == "eval":
            check(" ".join(args[1:]), cwd, depth + 1, substituted)
        elif prog in ("xargs", "nohup", "time", "timeout", "nice", "sudo", "command", "exec", "watch"):
            if any(a in ("gh", "git") or a.endswith(("/gh", "/git")) for a in args[1:]):
                block(f"gh/git wrapped in {prog} cannot be checked")
        elif prog == "export" and any(a.startswith(REDIRECTING_ENV) for a in args[1:]):
            block("export of GH_*/GIT_* variables")
        elif prog == "gh":
            check_gh(args[1:], ctx)
        elif prog == "git":
            check_git(args[1:], ctx)


def main() -> int:
    try:
        data = json.load(sys.stdin)
        cmd = (data.get("tool_input") or {}).get("command") or ""
        cwd = data.get("cwd") or ""
    except (ValueError, AttributeError):
        return 0
    try:
        check(cmd, cwd)
    except Blocked as e:
        print(f"BLOCKED by .claude/hooks/guard-git.sh: {e}. main/master are owner-only; target dev instead.",
              file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
