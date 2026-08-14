#!/usr/bin/env bash
# Re-apply the linux/amd64 platform trim to vendor/ after `go mod vendor`.
#
# Background: `go mod vendor` copies the FULL union of all supported
# platforms (modernc.org/sqlite+libc alone come back as ~130MB). This
# deployment only builds for linux/amd64 (glibc), so we delete every
# package/file that the current platform's build+test closure does not
# need, using `go list -json` as the authority for what is compiled.
#
# Usage:  go mod vendor && ./trim-vendor.sh
# Verify: go build -mod=vendor ./... && go test -mod=vendor -count=1 ./...
set -euo pipefail
cd "$(dirname "$0")"

python3 - <<'PY'
import json, os, shutil, subprocess

VENDOR = "vendor"

out = subprocess.run(["go", "list", "-mod=vendor", "-test", "-deps", "./..."],
                     capture_output=True, text=True, check=True)
closure = set(p for p in out.stdout.split() if p.startswith(("github.", "golang.", "modernc.")))

module_roots = []
with open(os.path.join(VENDOR, "modules.txt")) as f:
    for line in f:
        if line.startswith("# "):
            module_roots.append(line.split()[1])

def import_path_for_dir(d):
    for root in module_roots:
        rootdir = os.path.join(VENDOR, root)
        if os.path.commonpath([os.path.abspath(rootdir), os.path.abspath(d)]) == os.path.abspath(rootdir):
            rel = os.path.relpath(d, rootdir)
            return root if rel == "." else root + "/" + rel.replace(os.sep, "/")
    return None

pkg_dirs = []
for dirpath, dirnames, filenames in os.walk(VENDOR):
    dirnames[:] = [d for d in dirnames if not d.startswith(".")]
    if any(f.endswith(".go") for f in filenames):
        pkg_dirs.append(dirpath)

closure_dirs = {os.path.join(VENDOR, *ip.split('/')) for ip in closure
                if os.path.isdir(os.path.join(VENDOR, *ip.split('/')))}
ancestor_dirs = set()
for d in sorted(pkg_dirs, key=len, reverse=True):
    ip = import_path_for_dir(d)
    if ip in closure:
        continue
    if any(cd.startswith(d + os.sep) for cd in closure_dirs):
        # ancestor of a needed package: keep subdirs, drop its own files
        ancestor_dirs.add(d)
        for f in os.listdir(d):
            if f.endswith((".go", ".s", ".S")):
                os.remove(os.path.join(d, f))
        continue
    shutil.rmtree(d)
    print(f"  removed package {ip or d}")

for ip in sorted(closure):
    d = os.path.join(VENDOR, ip)
    if not os.path.isdir(d):
        continue
    out = subprocess.run(["go", "list", "-mod=vendor", "-json", ip],
                         capture_output=True, text=True)
    if out.returncode != 0:
        print(f"  WARN: go list failed for {ip}: {out.stderr.strip()[:120]}")
        continue
    info = json.loads(out.stdout)
    keep = set(info.get("GoFiles", [])) | set(info.get("CgoFiles", [])) | set(info.get("SFiles", []))
    for f in os.listdir(d):
        p = os.path.join(d, f)
        if not os.path.isfile(p) or f in keep or f.startswith("LICENSE"):
            continue
        if f.endswith((".go", ".s", ".S")):
            os.remove(p)

# Drop module docs/scripts/images that go mod vendor re-copies (cosmetic).
JUNK = {"README.md", "README", "README.markdown", "CHANGELOG.md", "CONTRIBUTING.md",
        "CLAUDE.md", "GEMINI.md", "GOVERNANCE.md", "HACKING.md", "Makefile", "builder.json",
        "logo.png", "bench.png", "surface.new", "surface.old", "tpch.sh", "unconvert.sh",
        "mkall.sh", "mkerrors.sh", "build_all_targets.sh", "issue120.diff",
        "nist-sts-2-1-1-report", ".gitignore", ".travis.yml", "embed.db", "embed2.db",
        "gccgo_c.c"}
for dirpath, dirnames, filenames in os.walk(VENDOR):
    dirnames[:] = [d for d in dirnames if not d.startswith(".")]
    for f in filenames:
        if f in JUNK:
            os.remove(os.path.join(dirpath, f))

print("vendor trimmed for", subprocess.run(["go", "env", "GOOS", "GOARCH"],
      capture_output=True, text=True).stdout.split())
PY
