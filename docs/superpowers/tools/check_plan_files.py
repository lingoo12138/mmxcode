"""Check the plan against the code that shipped.

Two things are checked.

The first is the whole files the plan says it carries: they have to be
in the plan byte for byte, so the plan cannot drift away from the repo.

The second is every declaration in the plan's Go blocks: each one has to
be in the repo as it is. A block that is a whole file has to parse; a
block that is a fragment cannot be parsed on its own, but its
declarations can still be looked for.

Usage: python -I check_plan_files.py <plan> <repo root>
"""
import io
import os
import re
import subprocess
import sys

DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|type\s+([A-Za-z_]\w*)|"
    r"var\s+([A-Za-z_]\w*)|const\s+([A-Za-z_]\w*))", re.M)
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

# The whole files the plan is supposed to carry: the ones this feature
# wrote from scratch. The files it only touched (store.go, webui.go and
# the rest) are carried as the part that changed, and those parts are
# checked declaration by declaration below.
WHOLE = [
    "internal/sign/sign.go",
    "internal/sign/sign_test.go",
    "internal/secret/secret.go",
    "internal/secret/secret_test.go",
    "internal/webauth/webauth.go",
    "internal/webauth/rsa.go",
    "internal/webauth/webauth_test.go",
    "internal/provision/provision.go",
    "internal/provision/provision_test.go",
    "internal/provision/chain.go",
    "internal/provision/chain_test.go",
    "cmd/mmxcode/import.go",
    "cmd/mmxcode/import_test.go",
    "internal/store/legacy_proxy.go",
    "internal/manager/proxy_groups.go",
    "internal/manager/proxy_groups_test.go",
]


def decl_name(line):
    m = DECL.match(line)
    if not m:
        return None
    for g in m.groups():
        if g:
            return g
    return None


def segments(text):
    """The top-level declarations of a piece of Go, with their text."""
    lines = text.split("\n")
    starts = [i for i, line in enumerate(lines) if decl_name(line)]
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
        stop = end
        while stop > i and (lines[stop - 1].strip().startswith("//")
                            or lines[stop - 1].strip().startswith("/*")
                            or lines[stop - 1].strip().endswith("*/")):
            stop -= 1
        out.append((decl_name(lines[i]), head, stop))
    return out


def main():
    plan_path, repo = sys.argv[1], sys.argv[2]
    plan = io.open(plan_path, encoding="utf-8").read()

    files = {}
    for root, dirs, names in os.walk(repo):
        dirs[:] = [d for d in dirs if d not in (".git", ".worktrees", "docs")]
        for n in names:
            if n.endswith(".go"):
                p = os.path.join(root, n)
                files[os.path.relpath(p, repo).replace(os.sep, "/")] = \
                    io.open(p, encoding="utf-8").read()

    blocks = re.findall(r"```go\n(.*?)\n```", plan, re.S)

    # A whole file has to parse on its own.
    unparseable = 0
    for i, b in enumerate(blocks):
        if not is_whole_file(b):
            continue
        tmp = os.path.join(os.environ.get("TMP", "/tmp"), "block_%d.go" % i)
        io.open(tmp, "w", encoding="utf-8", newline="\n").write(b + "\n")
        p = subprocess.run(["gofmt", "-e", tmp], capture_output=True)
        if p.returncode != 0:
            unparseable += 1
            print("block %d does not parse: %s"
                  % (i, p.stderr.decode("utf-8", "replace").strip()[:200]))

    # Every declaration in the plan has to be in the repo as it is.
    stale = 0
    for i, b in enumerate(blocks):
        lines = b.split("\n")
        for name, head, stop in segments(b):
            text = "\n".join(lines[head:stop]).rstrip("\n")
            if not text:
                continue
            if any(text in t for t in files.values()):
                continue
            stale += 1
            print("block %d: %s is not in the repo as it is" % (i, name))

    # The whole files the plan says it carries.
    for w in WHOLE:
        path = os.path.join(repo, w.replace("/", os.sep))
        if not os.path.exists(path):
            print("not in the repo yet: " + w)
            continue
        text = io.open(path, encoding="utf-8").read().rstrip("\n")
        if text not in plan:
            print("plan does not carry %s verbatim" % w)

    print("blocks: %d, unparseable: %d, stale declarations: %d"
          % (len(blocks), unparseable, stale))


if __name__ == "__main__":
    main()
