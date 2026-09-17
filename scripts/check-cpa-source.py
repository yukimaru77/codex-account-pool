#!/usr/bin/env python3
"""Report changes to the narrow CPA sources we use; never update either checkout."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout")
    parser.add_argument("--ref", default="HEAD")
    args = parser.parse_args()
    manifest = json.loads((Path(__file__).resolve().parents[1] / "third_party/cpa/sources.json").read_text())
    git = ["git", "-C", args.checkout]
    commit = subprocess.check_output(git + ["rev-parse", "--verify", "--end-of-options", args.ref + "^{commit}"], text=True).strip()
    changed = 0
    print("CPA reference:", commit)
    for file in manifest["files"]:
        result = subprocess.run(git + ["show", commit + ":" + file["source"]], capture_output=True)
        if result.returncode:
            status = "MISSING"
        else:
            status = "unchanged" if hashlib.sha256(result.stdout).hexdigest() == file["source_sha256"] else "CHANGED"
        if status != "unchanged":
            changed += 1
        print(status + ": " + file["source"])
    print(str(changed) + " of " + str(len(manifest["files"])) + " source files changed")


if __name__ == "__main__":
    main()
