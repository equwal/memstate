#!/usr/bin/env python3
"""Hermetic test of scratch-folder naming for the CLI scripts.

Each script runs with its working directory in a folder named like
"scratch-2026-01-02-abc123", and spawns its own daemon (child mode) on a
temporary database. Run: python client/skill/scripts/test_scratch_naming.py
"""
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
NAME = "regress_naming_task"
failures = 0


def check(name: str, cond: bool, detail: str = "") -> None:
    global failures
    if cond:
        print(f"  ok  {name}")
    else:
        failures += 1
        print(f"FAIL  {name} - {detail.strip()[:300]}")


def run(script: str, *args: str, cwd: Path, env: dict):
    p = subprocess.run(
        [sys.executable, str(SCRIPTS / script), *args],
        cwd=cwd, env=env, capture_output=True, text=True, timeout=60,
    )
    try:
        data = json.loads(p.stdout)
    except json.JSONDecodeError:
        data = None
    return p.returncode, data, p.stderr


def main() -> int:
    with tempfile.TemporaryDirectory(ignore_cleanup_errors=True) as tmp_name:
        tmp = Path(tmp_name)
        env = dict(os.environ)
        env.pop("MEMSTATE_ADDR", None)  # force child mode
        env.pop("MEMSTATE_LOCAL_URL", None)
        env["MEMSTATE_DB"] = str(tmp / "test.db")
        env["MEMSTATE_NO_UPDATE_CHECK"] = "1"
        env["MEMSTATE_OLLAMA_URL"] = "http://127.0.0.1:9"  # closed port
        dir_a = tmp / "scratch-2026-01-02-abc123"
        dir_b = tmp / "scratch-2026-01-02-def456"
        dir_a.mkdir()
        dir_b.mkdir()

        code, _, err = run("memstate_set.py", "--keypath", "notes.first",
                           "--value", "a", cwd=dir_a, env=env)
        check("first write without a name is refused",
              code != 0 and "--project-name" in err, err)

        code, _, err = run("memstate_set.py", "--keypath", "notes.first",
                           "--value", "a", "--project-name", "Bad Name",
                           cwd=dir_a, env=env)
        check("an invalid name is refused", code != 0 and "must be" in err, err)

        code, data, err = run("memstate_set.py", "--keypath", "notes.first",
                              "--value", "a", "--project-name", NAME,
                              cwd=dir_a, env=env)
        check("--project-name names the project on the first write",
              code == 0 and data["stored"]["project_id"] == NAME, err)

        code, data, err = run("memstate_remember.py", "--keypath", "notes.second",
                              "--content", "b", cwd=dir_a, env=env)
        check("later writes use the stored name",
              code == 0 and data["items"][0]["stored"]["project_id"] == NAME, err)

        code, data, err = run("memstate_get.py", cwd=dir_a, env=env)
        check("reads use the stored name",
              code == 0 and data["project_id"] == NAME
              and data["total_memories"] == 2, err)

        code, data, err = run("memstate_set.py", "--keypath", "notes.first",
                              "--value", "c", "--project-name", NAME,
                              cwd=dir_b, env=env)
        check("a name in use gets a number",
              code == 0 and data["stored"]["project_id"] == f"{NAME}_2", err)

        code, _, err = run("memstate_set.py", "--project", "plain_project",
                           "--keypath", "x.y", "--value", "z",
                           "--project-name", NAME, cwd=tmp, env=env)
        check("--project-name outside a scratch folder is refused",
              code != 0 and "scratch folder" in err, err)

    if failures:
        print(f"\n{failures} check(s) FAILED")
        return 1
    print("\nall scratch naming checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
