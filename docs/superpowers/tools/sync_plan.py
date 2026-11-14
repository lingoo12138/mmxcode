"""Put the repo's files into the plan, in place of the blocks that carry them.

The plan is the contract: its code must be the code that ships. When a
file changes after the plan was written, the plan's copy of it is stale.
This finds the block that carries each file and replaces its contents with
the file as it is now.

Which block carries which file is worked out from the top-level
declarations: a block that is the whole of some file has that file's
declarations, and a fragment does not. So only whole-file blocks are
touched, and the ones describing a change stay as they are.

Usage: python -I sync_plan.py <plan> <file> [<file> ...]
"""
import io
import os
import re
import sys

DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|type\s+([A-Za-z_]\w*)|"
    r"var\s+([A-Za-z_]\w*)|const\s+([A-Za-z_]\w*))",
    re.M,
)
PKG = re.compile(r"(?m)^package\s+(\w+)")


def decls(text):
    """The top-level names a piece of Go declares."""
    out = set()
    for m in DECL.finditer(text):
        for g in m.groups():
            if g:
                out.add(g)
    return out


def score(a, b):
    """How much two sets of declarations have in common."""
    if not a or not b:
        return 0.0
    return len(a & b) / float(len(a | b))


def pkg_of(text):
    m = PKG.search(text)
    return m.group(1) if m else ""


def whole_file_blocks(plan):
    """The plan's blocks that carry a whole file.

    A whole file starts with its package clause, give or take the
    comment above it. A fragment does not have one at all.
    """
    out = []
    for m in re.finditer(r"```go\n(.*?)\n```", plan, re.S):
        body = m.group(1)
        p = pkg_of(body)
        if not p:
            continue
        # The package clause has to be near the top: a block that
        # mentions a package halfway down is a fragment.
        if body[:body.index("package ")].count("\n") >= 30:
            continue
        out.append([m.start(1), m.end(1), body, decls(body), p])
    return out


def resolve(path):
    if not os.path.exists(path):
        return path
    return path


def main():
    plan_path = sys.argv[1]
    files = sys.argv[2:]

    plan = io.open(plan_path, encoding="utf-8").read()
    spans = whole_file_blocks(plan)

    pairs = []
    for f in files:
        path = resolve(f)
        if not os.path.exists(path):
            print("not found: " + f)
            continue
        text = io.open(path, encoding="utf-8").read()
        fd, fp = decls(text), pkg_of(text)
        for i, s in enumerate(spans):
            if s[4] != fp:
                continue
            pairs.append((score(fd, s[3]), f, i))
    pairs.sort(key=lambda p: -p[0])

    taken_file, taken_block = set(), set()
    chosen = {}
    for sc, f, i in pairs:
        if f in taken_file or i in taken_block:
            continue
        if sc < 0.4:
            continue
        taken_file.add(f)
        taken_block.add(i)
        chosen[f] = i

    for f in files:
        if f not in chosen:
            print("no block found for %s" % f)

    # Replace from the back, so the earlier spans keep their positions.
    out = plan
    for f, i in sorted(chosen.items(), key=lambda kv: -kv[1]):
        code = io.open(resolve(f), encoding="utf-8").read().rstrip("\n")
        s = spans[i]
        out = out[:s[0]] + code + out[s[1]:]

    io.open(plan_path, "w", encoding="utf-8", newline="\n").write(out)
    print("synced %d file(s)" % len(chosen))


if __name__ == "__main__":
    main()
