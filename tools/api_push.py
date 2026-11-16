#!/usr/bin/env python3
"""Push the local working tree to GitHub through the Git Data API.

Use when `git push` dies at the transport layer ("expected flush after ref
listing" / "unexpected disconnect while reading sideband packet") but
api.github.com still answers. Creates blobs for every tracked path, one tree,
one commit and then moves/creates the branch ref.

Usage: python api_push.py <owner/repo> <branch> <commit-message-file>
"""
import base64
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

repo, branch, msgfile = sys.argv[1], sys.argv[2], sys.argv[3]
REPO_DIR = r"C:/Users/Zcc09/twinlink"

cfg = os.path.join(os.environ.get("LOCALAPPDATA", ""), "hermes", "config.yaml")
tok = ""
with open(cfg, encoding="utf-8", errors="replace") as fh:
    for line in fh:
        if "GITHUB_PERSONAL_ACCESS_TOKEN" in line:
            tok = line.split(":", 1)[-1].strip().strip('"').strip("'")
            break
H = {"Authorization": "token " + tok, "Accept": "application/vnd.github+json"}


def api(path, payload=None, method=None, absolute=False):
    url = path if absolute else f"https://api.github.com/repos/{repo}/{path}"
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, headers={**H, "Content-Type": "application/json"},
                                 method=method or ("POST" if data is not None else "GET"))
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode()[:500]}


def tracked_files():
    out = subprocess.run(["git", "-C", REPO_DIR, "ls-files"], capture_output=True, text=True, check=True)
    return [p for p in out.stdout.splitlines() if p.strip()]


def main():
    files = tracked_files()
    print(f"{len(files)} tracked files")
    entries = []
    for path in files:
        full = os.path.join(REPO_DIR, path.replace("/", os.sep))
        with open(full, "rb") as fh:
            content = fh.read()
        st, blob = api("git/blobs", {"content": base64.b64encode(content).decode(), "encoding": "base64"})
        if st not in (200, 201):
            print("blob FAILED", path, blob)
            return 1
        entries.append({"path": path, "mode": "100644", "type": "blob", "sha": blob["sha"]})
    print("blobs done")

    st, tree = api("git/trees", {"tree": entries})
    if st not in (200, 201):
        print("tree FAILED", tree)
        return 1
    print("tree", tree["sha"])

    # branch may not exist yet (empty repo)
    st, ref = api(f"git/ref/heads/{branch}")
    parents = [ref["object"]["sha"]] if st == 200 else []
    message = open(msgfile, encoding="utf-8").read()
    st, commit = api("git/commits", {"message": message, "tree": tree["sha"], "parents": parents})
    if st not in (200, 201):
        print("commit FAILED", commit)
        return 1
    print("commit", commit["sha"])

    if parents:
        st, res = api(f"git/refs/heads/{branch}", {"sha": commit["sha"], "force": False}, method="PATCH")
    else:
        st, res = api(f"git/refs", {"ref": f"refs/heads/{branch}", "sha": commit["sha"]})
    print("ref update:", st, str(res)[:200])
    return 0 if st in (200, 201) else 1


sys.exit(main())
