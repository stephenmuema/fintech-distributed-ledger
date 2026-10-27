"""Writes a findings count for every *.sarif file in the working directory to the job summary."""

import glob
import json
import os

lines = ["| Report | Findings |", "|---|---:|"]
for path in sorted(glob.glob("*.sarif")):
    with open(path, encoding="utf-8") as f:
        runs = json.load(f).get("runs", [])
    lines.append(f"| {path} | {sum(len(run.get('results', [])) for run in runs)} |")

with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as out:
    print(*lines, sep="\n", file=out)
