"""Check that the plan's Go blocks hold Go and nothing else.

A block of Go that has a line of prose in it is a block that will not
compile, and the error it gives says nothing about where the prose
came from. This looks for the lines that are not Go.

Usage: python -I check_plan_fences.py <plan>
"""
import io
import re
import sys

plan_path = sys.argv[1]
text = io.open(plan_path, encoding="utf-8").read().replace("\r\n", "\n")

blocks = re.findall(r"```(\w*)\n(.*?)\n```", text, re.S)
problems = []
for lang, body in blocks:
    if lang != "go":
        continue
    ticks = 0
    for i, line in enumerate(body.split("\n"), 1):
        s = line.strip()
        # A line inside a raw string is a string, whatever it looks like.
        raw = (ticks % 2 == 1)
        if not raw and s.startswith("//"):
            ticks += line.count("`")
            continue
        # Chinese outside a string is prose. A string is easy to spot:
        # the line has a quote in it.
        if not raw and re.search(r"[一-鿿]", line) and '"' not in line:
            problems.append((i, line))
        # A tick or a box is not Go either.
        if not raw and re.search(r"[✔✗→←]", line) and '"' not in line:
            problems.append((i, line))
        ticks += line.count("`")

out = io.open(sys.argv[2], "w", encoding="utf-8", newline="\n") if len(sys.argv) > 2 else None
lines = ["%d: %s" % (i, l[:120]) for i, l in problems]
text_out = "\n".join(lines) + "\n%d problem(s)\n" % len(problems)
if out:
    out.write(text_out)
    print("%d problem(s) written to %s" % (len(problems), sys.argv[2]))
else:
    print(text_out.encode("ascii", "replace").decode("ascii"))
