#!/usr/bin/env python3
"""Fetch and verify a previously validated Linux Native AOT gateway artifact.

The command never compiles on the production host. It reuses the exact
linux-musl-native-gateway artifact for a successful CSharp Linux Native AOT
workflow run whose head SHA matches the requested source commit.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
from typing import Any


WORKFLOW = "csharp-native-aot.yml"
ARTIFACT = "linux-musl-native-gateway"
BINARY = "HttpMigration"


def checked_bundle_name(value: Any) -> str:
    if (not isinstance(value, str) or not value or "/" in value or "\\" in value
            or Path(value).name != value):
        raise RuntimeError("cached manifest has an invalid bundle name")
    return value


def command(*args: str, cwd: Path | None = None) -> str:
    result = subprocess.run(args, cwd=cwd, check=True, text=True,
                            encoding="utf-8", errors="replace",
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return result.stdout


def gh_json(endpoint: str) -> Any:
    return json.loads(command("gh", "api", endpoint))


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def verify_bundle(bundle: Path, sums: Path) -> dict[str, Any]:
    if not bundle.is_file() or not sums.is_file():
        raise RuntimeError("artifact must contain the bundle and SHA256SUMS")
    entries = sums.read_text(encoding="utf-8").splitlines()
    matching = [line.split() for line in entries if len(line.split()) == 2 and line.split()[1] == BINARY]
    if len(matching) != 1 or len(matching[0][0]) != 64:
        raise RuntimeError("SHA256SUMS must contain exactly one HttpMigration entry")
    expected_binary = matching[0][0].lower()
    if not bundle.name.endswith(".tar.gz"):
        raise RuntimeError("gateway artifact must be a tar.gz bundle")
    with tarfile.open(bundle, "r:gz") as archive:
        members = archive.getmembers()
        if len(members) != 1 or members[0].name != BINARY or not members[0].isreg() or not members[0].mode & 0o111:
            raise RuntimeError("bundle must contain one executable named HttpMigration")
        binary = archive.extractfile(members[0])
        if binary is None:
            raise RuntimeError("bundle executable cannot be read")
        data = binary.read()
    actual_binary = hashlib.sha256(data).hexdigest()
    if actual_binary != expected_binary:
        raise RuntimeError(f"binary SHA256 mismatch: {actual_binary} != {expected_binary}")
    if data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 18)[0] != 62:
        raise RuntimeError("artifact is not an ELF x86-64 executable")
    return {"binarySha256": actual_binary, "binaryBytes": len(data), "bundleSha256": sha256(bundle), "bundleBytes": bundle.stat().st_size}


def verify_smoke(path: Path) -> dict[str, Any]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
        result = next(json.loads(line) for line in reversed(lines) if line.lstrip().startswith("{"))
    except (OSError, json.JSONDecodeError, StopIteration) as error:
        raise RuntimeError("smoke.log is missing or invalid") from error
    if result.get("gateway") != "passed" or result.get("modelRequests") != 6 or result.get("toolRequests") != 2 or result.get("persistedSyntheticCredits") != 12:
        raise RuntimeError("AOT smoke evidence does not match the expected offline acceptance")
    return result


def source_commit() -> str:
    return command("git", "rev-parse", "HEAD").strip()


def successful_runs(repo: str, commit: str) -> list[dict[str, Any]]:
    result = gh_json(f"repos/{repo}/actions/workflows/{WORKFLOW}/runs?head_sha={commit}&per_page=30")
    return sorted(
        [run for run in result.get("workflow_runs", [])
         if run.get("head_sha") == commit and run.get("status") == "completed" and run.get("conclusion") == "success"],
        key=lambda run: run.get("created_at", ""), reverse=True)


def wait_for_run(repo: str, commit: str, timeout: int) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        runs = successful_runs(repo, commit)
        if runs:
            return runs[0]
        time.sleep(10)
    raise RuntimeError(f"timed out waiting for {WORKFLOW} for {commit}")


def trigger(repo: str, commit: str, timeout: int) -> dict[str, Any]:
    branch = command("git", "branch", "--show-current").strip()
    if not branch:
        raise RuntimeError("--build-if-missing requires a checked-out branch")
    branches = gh_json(f"repos/{repo}/commits/{commit}/branches-where-head?per_page=100")
    if not any(item.get("name") == branch for item in branches):
        raise RuntimeError("--build-if-missing requires the checked-out branch to be pushed at the requested SHA")
    command("gh", "workflow", "run", WORKFLOW, "--repo", repo, "--ref", branch)
    return wait_for_run(repo, commit, timeout)


def reuse(output: Path, commit: str) -> dict[str, Any] | None:
    manifest_path = output / "release.json"
    if not manifest_path.exists():
        if output.exists() and any(output.iterdir()):
            raise RuntimeError(f"output directory is non-empty but has no release.json: {output}")
        return None
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if manifest.get("sourceCommit") != commit:
        raise RuntimeError(f"cached release is for {manifest.get('sourceCommit')}, requested {commit}")
    if manifest.get("artifact") != ARTIFACT or manifest.get("workflow") != WORKFLOW:
        raise RuntimeError("cached manifest does not name the verified gateway workflow artifact")
    bundle = output / checked_bundle_name(manifest.get("bundleName"))
    actual = verify_bundle(bundle, output / "SHA256SUMS")
    smoke = verify_smoke(output / "smoke.log")
    for field, value in actual.items():
        if manifest.get(field) != value:
            raise RuntimeError(f"cached release manifest mismatch for {field}")
    if manifest.get("smoke") != smoke:
        raise RuntimeError("cached smoke evidence differs from release manifest")
    return manifest


def download(repo: str, run_id: int, destination: Path) -> tuple[Path, Path, Path | None]:
    destination.mkdir(parents=True, exist_ok=True)
    command("gh", "run", "download", str(run_id), "--repo", repo, "--name", ARTIFACT, "--dir", str(destination))
    bundles = list(destination.glob("*.tar.gz"))
    sums = destination / "SHA256SUMS"
    smoke = destination / "smoke.log"
    if len(bundles) != 1 or not sums.is_file():
        raise RuntimeError("downloaded artifact has an unexpected layout")
    return bundles[0], sums, smoke if smoke.is_file() else None


def prepare(args: argparse.Namespace) -> dict[str, Any]:
    repo = args.repo
    commit = args.commit or source_commit()
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise RuntimeError("source commit must be a full lowercase Git SHA")
    output = args.output.resolve() if args.output else (Path("out/releases") / commit[:12]).resolve()
    cached = reuse(output, commit)
    if cached is not None:
        return {**cached, "reusedLocalArtifact": True}

    run = None
    if args.run_id:
        candidate = gh_json(f"repos/{repo}/actions/runs/{args.run_id}")
        workflow = gh_json(f"repos/{repo}/actions/workflows/{WORKFLOW}")
        if (candidate.get("head_sha") != commit
                or candidate.get("conclusion") != "success"
                or candidate.get("workflow_id") != workflow.get("id")):
            raise RuntimeError("requested run is not a successful run of the expected workflow for the requested commit")
        run = candidate
    else:
        runs = successful_runs(repo, commit)
        run = runs[0] if runs else None
        if run is None and args.build_if_missing:
            run = trigger(repo, commit, args.wait_seconds)
    if run is None:
        raise RuntimeError(f"no successful {WORKFLOW} artifact exists for {commit}; use --build-if-missing")

    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="aot-release-", dir=output.parent) as temporary:
        staging = Path(temporary) / "download"
        staging.mkdir()
        bundle, sums, smoke_path = download(repo, int(run["id"]), staging)
        evidence = verify_bundle(bundle, sums)
        if smoke_path is None:
            raise RuntimeError("downloaded artifact is missing smoke.log")
        smoke_evidence = verify_smoke(smoke_path)
        prepared = Path(temporary) / "prepared"
        prepared.mkdir()
        target_bundle = prepared / bundle.name
        shutil.copy2(bundle, target_bundle)
        shutil.copy2(sums, prepared / sums.name)
        shutil.copy2(smoke_path, prepared / smoke_path.name)
        manifest = {
            "repository": repo,
            "sourceCommit": commit,
            "workflow": WORKFLOW,
            "runId": int(run["id"]),
            "artifact": ARTIFACT,
            "bundleName": bundle.name,
            "verifiedAt": dt.datetime.now(dt.timezone.utc).isoformat(),
            "smoke": smoke_evidence,
            **evidence,
        }
        (prepared / "release.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        if output.exists():
            output.rmdir()
        prepared.replace(output)
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", default=os.environ.get("GITHUB_REPOSITORY", "GMagisk9527/workbuddy-gateway"))
    parser.add_argument("--commit", help="exact source SHA; defaults to HEAD")
    parser.add_argument("--run-id", type=int, help="use this already successful Actions run")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--build-if-missing", action="store_true", help="dispatch the workflow for this exact SHA")
    parser.add_argument("--wait-seconds", type=int, default=3600)
    args = parser.parse_args()
    if args.wait_seconds < 60 or args.wait_seconds > 7200:
        parser.error("--wait-seconds must be between 60 and 7200")
    try:
        print(json.dumps(prepare(args), indent=2))
    except (OSError, RuntimeError, subprocess.CalledProcessError, json.JSONDecodeError) as error:
        print(f"release preparation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
