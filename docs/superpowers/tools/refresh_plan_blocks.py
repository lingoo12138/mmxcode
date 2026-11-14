"""Refresh the plan's Go blocks from the code that shipped.

The plan is the contract: the code in it is the code that runs. When the
implementation turns out slightly different from the plan — a helper
pulled out here, a name changed there — the plan has to be brought back
into line, not left lying.

This goes through the plan's Go blocks a declaration at a time. A
declaration is looked up in the repo by its name and its first line, and
if the repo has a different version of it, the plan's copy is replaced
with the repo's. Declarations the repo does not have are left alone and
reported, because those are the ones a person has to look at.

Usage: python -I refresh_plan_blocks.py <plan> <repo root>
"""
import io
import os
import re
import sys

# A top-level declaration: a function (with or without a receiver), a
# type, a var or a const.
DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|type\s+([A-Za-z_]\w*)|"
    r"var\s+([A-Za-z_]\w*)|const\s+([A-Za-z_]\w*))", re.M)
IDENT = re.compile(r"[A-Za-z_]\w*")
PKG = re.compile(r"(?m)^package\s+\w+")


def is_whole_file(body):
    """Is this block the whole of a file?

    A whole file has a package clause near the top -- give or take the
    comment above it. A fragment has none.
    """
    m = PKG.search(body)
    if not m:
        return False
    return body[:m.start()].count("\n") < 30


def decl_name(line):
    m = DECL.match(line)
    if not m:
        return None
    for g in m.groups():
        if g:
            return g
    return None


def head_line(line):
    """The first line of a declaration, without its braces.

    Two versions of the same declaration differ in the body, not in how
    they start, so this is what identifies them.
    """
    return line.rstrip().rstrip("{").rstrip().strip()


def segments(text):
    """Split a piece of Go into its top-level declarations.

    Each segment is the declaration with the comment above it, and
    returns where it starts and ends in the text, its name, and its
    first line.
    """
    lines = text.split("\n")
    starts = []
    for i, line in enumerate(lines):
        if decl_name(line):
            starts.append(i)

    out = []
    for n, i in enumerate(starts):
        head = i
        while head > 0:
            prev = lines[head - 1].strip()
            if prev.startswith("//") or prev.startswith("/*") or prev.endswith("*/"):
                head -= 1
                continue
            break
        end = starts[n + 1] if n + 1 < len(starts) else len(lines)
        # The next declaration's comment belongs to the next
        # declaration, so this one ends where that comment begins.
        stop = end
        while stop > i and (lines[stop - 1].strip().startswith("//")
                            or lines[stop - 1].strip().startswith("/*")
                            or lines[stop - 1].strip().endswith("*/")):
            stop -= 1
        out.append({
            "name": decl_name(lines[i]),
            "first": head_line(lines[i]),
            "start": head,
            "end": stop,
        })
    return out


def file_segments(text):
    """The same, for a whole file: the segments and their text."""
    segs = segments(text)
    lines = text.split("\n")
    for s in segs:
        s["text"] = "\n".join(lines[s["start"]:s["end"]])
    return segs


def main():
    plan_path, repo = sys.argv[1], sys.argv[2]
    plan = io.open(plan_path, encoding="utf-8").read()

    files = []
    for root, dirs, names in os.walk(repo):
        dirs[:] = [d for d in dirs if d not in (".git", ".worktrees", "docs")]
        for n in names:
            if n.endswith(".go"):
                p = os.path.join(root, n)
                files.append((os.path.relpath(p, repo).replace(os.sep, "/"),
                              io.open(p, encoding="utf-8").read()))

    def find(name, first):
        """The repo's version of a declaration, if it has one."""
        by_name = []
        for fname, text in files:
            for s in file_segments(text):
                if s["name"] != name:
                    continue
                if s["first"] == first:
                    return fname, s["text"]
                by_name.append((fname, s["text"]))
        if len(by_name) == 1:
            return by_name[0]
        return None, None

    out = plan
    changed = 0
    missing = []

    for m in reversed(list(re.finditer(r"```go\n(.*?)\n```", out, re.S))):
        body = m.group(1)
        # A block that is a whole file is kept byte for byte by
        # sync_plan.py; leave it alone here.
        if is_whole_file(body):
            continue
        segs = segments(body)
        if not segs:
            continue
        lines = body.split("\n")
        pieces = []
        for s in segs:
            text = "\n".join(lines[s["start"]:s["end"]])
            fname, fresh = find(s["name"], s["first"])
            if fresh is None:
                fname, fresh = find(s["name"], "")
            if fresh is None:
                missing.append((s["name"], s["first"][:60]))
                pieces.append(text)
            else:
                if fresh.rstrip("\n") != text.rstrip("\n"):
                    changed += 1
                pieces.append(fresh)
        new = "\n".join(pieces)
        out = out[:m.start(1)] + new + out[m.end(1):]

    io.open(plan_path, "w", encoding="utf-8", newline="\n").write(out)
    for name, first in missing:
        print("not in the repo: %s  (%s)" % (name, first))
    print("%d declaration(s) refreshed, %d not found" % (changed, len(missing)))


if __name__ == "__main__":
    main()
