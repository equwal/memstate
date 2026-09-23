"""Shared HTTP client for the memstate CLI scripts.

Two modes, mirroring the TS proxy:

  * attach  — MEMSTATE_ADDR is set: talk to a daemon someone else started.
              We never spawn, never kill.
  * child   — MEMSTATE_ADDR is unset: spawn `memstated --owner-pid=<us>`,
              read the "MEMSTATE_READY addr=..." banner from its stderr,
              SIGTERM on script exit. 1:1 lifetime with this Python process.

The child is cached on the module, so a single script that imports this
module and makes several requests reuses one daemon.

Env:
  MEMSTATE_ADDR       attach to this host:port (attach mode)
  MEMSTATE_BIN        override the daemon path (default: sibling build / PATH)
  MEMSTATE_LOCAL_URL  full base URL override (for both modes)
"""
import atexit
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Optional

READY_RE = re.compile(r"MEMSTATE_READY addr=(\S+)")
_READY_TIMEOUT = 5.0

_child: Optional[subprocess.Popen] = None
_base_url: Optional[str] = None
_started_lock = threading.Lock()


def _resolve_bin() -> str:
    explicit = os.environ.get("MEMSTATE_BIN")
    if explicit and Path(explicit).exists():
        return explicit
    # scripts/ → client/skill/scripts/ → ../../../server/memstated
    sibling = (Path(__file__).resolve().parent / ".." / ".." / ".." / "server" / "memstated").resolve()
    if sibling.exists():
        return str(sibling)
    return "memstated"  # fall through to PATH


def _spawn_child() -> str:
    """Spawn memstated, read banner, wire atexit cleanup. Returns addr."""
    global _child
    bin_path = _resolve_bin()
    log_path = Path.home() / ".memstate" / "memstated.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_fd = open(log_path, "a")

    # NOT detached — keep the child in our process group so SIGINT on the
    # terminal propagates, and .terminate() is authoritative. --owner-pid is
    # the safety net if we get SIGKILLed.
    child = subprocess.Popen(
        [bin_path, "--owner-pid", str(os.getpid())],
        stdin=subprocess.DEVNULL,
        stdout=log_fd,
        stderr=subprocess.PIPE,
        start_new_session=False,
    )
    _child = child

    addr: Optional[str] = None
    deadline = time.monotonic() + _READY_TIMEOUT
    assert child.stderr is not None
    while time.monotonic() < deadline:
        line = child.stderr.readline()
        if not line:
            break
        try:
            text = line.decode("utf-8", errors="replace")
        except Exception:
            text = ""
        # Tee to log so we don't lose banner or subsequent lines.
        log_fd.write(text)
        log_fd.flush()
        m = READY_RE.search(text)
        if m:
            addr = m.group(1).strip()
            break

    if addr is None:
        child.kill()
        child.wait(timeout=2)
        raise RuntimeError(
            f"memstated never printed READY banner within {_READY_TIMEOUT}s; see {log_path}"
        )

    # Drain the rest of stderr in background so the child never blocks on a
    # full pipe buffer. Each line goes to the log.
    def _drain() -> None:
        try:
            assert child.stderr is not None
            for chunk in child.stderr:
                log_fd.write(chunk.decode("utf-8", errors="replace"))
                log_fd.flush()
        except Exception:
            pass

    threading.Thread(target=_drain, daemon=True).start()

    atexit.register(_cleanup_child)
    # SIGINT / SIGTERM → run atexit (Python default) then let the signal
    # actually terminate us. We intercept to make sure we reap the child.
    def _on_signal(signum: int, _frame) -> None:
        _cleanup_child()
        # Restore default and re-raise so exit code reflects the signal.
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)
    for s in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(s, _on_signal)
        except (ValueError, OSError):
            # Not main thread / not supported: rely on atexit.
            pass

    return addr


def _cleanup_child() -> None:
    global _child
    c = _child
    if c is None or c.poll() is not None:
        return
    try:
        c.terminate()
        try:
            c.wait(timeout=2)
        except subprocess.TimeoutExpired:
            c.kill()
            c.wait(timeout=1)
    except Exception:
        pass
    _child = None


def _base() -> str:
    global _base_url
    if _base_url is not None:
        return _base_url
    explicit_url = os.environ.get("MEMSTATE_LOCAL_URL")
    if explicit_url:
        _base_url = explicit_url.rstrip("/")
        return _base_url
    attach_addr = os.environ.get("MEMSTATE_ADDR")
    if attach_addr:
        _base_url = f"http://{attach_addr}/api/v1"
        return _base_url
    # Child mode: spawn exactly once (thread-safe via the lock).
    with _started_lock:
        if _child is None:
            addr = _spawn_child()
        else:
            addr = ""  # unreachable
        _base_url = f"http://{addr}/api/v1"
    return _base_url


_HEADERS = {"Content-Type": "application/json"}


def _request(method: str, path: str, body: Optional[dict] = None) -> int:
    url = f"{_base()}{path}"
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=_HEADERS, method=method)
    try:
        with urllib.request.urlopen(req) as resp:
            payload = resp.read().decode("utf-8")
            try:
                print(json.dumps(json.loads(payload), indent=2))
            except json.JSONDecodeError:
                print(payload)
            return 0
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", errors="replace")
        print(f"Error: HTTP {e.code} {detail}", file=sys.stderr)
        return 1
    except urllib.error.URLError as e:
        print(
            f"Error: could not reach memstated at {_base_url}: {e.reason}",
            file=sys.stderr,
        )
        return 2


def derived_project() -> str:
    """Project id derived from the git repo name (or cwd basename outside a
    repo), slugged to lowercase snake_case — same rule as the TS proxy, so
    scripts and MCP sessions land in the same project."""
    base = ""
    try:
        top = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            capture_output=True, text=True, timeout=5,
        )
        if top.returncode == 0:
            base = Path(top.stdout.strip()).name
    except Exception:
        pass
    if not base:
        base = Path.cwd().name
    slug = re.sub(r"[^a-z0-9]+", "_", base.lower()).strip("_")
    return slug or "default"


# A session without a project folder runs in a temporary folder named like
# "scratch-2026-09-22-19fd5b". Its derived id is new for each session, so the
# first write names the project. The name is stored as a memory in
# ALIAS_PROJECT, the same record that the TS proxy reads and writes.
SCRATCH_ID_RE = re.compile(r"scratch_\d{4}_\d{2}_\d{2}_[0-9a-f]+")
PROJECT_NAME_RE = re.compile(r"[a-z][a-z0-9]*(_[a-z0-9]+){1,3}")
ALIAS_PROJECT = "project_aliases"
MAX_NAME_LENGTH = 40
MAX_NAME_NUMBER = 99

NAME_RULES = (
    "--project-name must be 2 to 4 lowercase words joined by underscores "
    f"(a-z and 0-9 only), at most {MAX_NAME_LENGTH} characters, and must "
    'not contain "scratch".'
)
NAMING_STEPS = (
    "this folder is a scratch folder, and its project has no name yet. "
    "Pass --project-name NAME on the first write. NAME tells the task in "
    f"2 to 4 lowercase words joined by underscores, at most {MAX_NAME_LENGTH} "
    'characters, no dates, no "scratch". The scripts store the name and use '
    "it for every later call from this folder. If another project already "
    "uses the name, a number is added to it."
)


def _fetch(method: str, path: str, body: Optional[dict] = None) -> dict:
    """Send one request and return the parsed JSON. Raises HTTPError."""
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(f"{_base()}{path}", data=data,
                                 headers=_HEADERS, method=method)
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read().decode("utf-8"))


def _is_scratch(pid: str) -> bool:
    return SCRATCH_ID_RE.fullmatch(pid) is not None


def session_name() -> str:
    """Stored name of this scratch folder, or "" when there is none."""
    pid = derived_project()
    if not _is_scratch(pid):
        return ""
    keypath = f"aliases.{pid}"
    try:
        res = _fetch("POST", "/keypaths", {"project_id": ALIAS_PROJECT,
                                           "keypath": keypath,
                                           "include_content": True})
    except (urllib.error.URLError, ValueError) as e:
        print(f"Warning: cannot read the scratch folder name: {e}", file=sys.stderr)
        return ""
    for m in res.get("memories") or []:
        if m.get("keypath") == keypath:
            return str(m.get("content", "")).strip()
    return ""


def default_project() -> str:
    """The stored name of a scratch folder, else the derived project id."""
    return session_name() or derived_project()


def valid_project_name(name: str) -> bool:
    return (len(name) <= MAX_NAME_LENGTH
            and PROJECT_NAME_RE.fullmatch(name) is not None
            and "scratch" not in name
            and name != ALIAS_PROJECT)


def _name_in_use(name: str) -> bool:
    """True when a live or a soft-deleted project has this id. A write to a
    soft-deleted project revives its old memories."""
    projects = _fetch("GET", "/projects").get("projects") or []
    if any(p.get("id") == name for p in projects):
        return True
    try:
        _fetch("GET", f"/tree?project_id={urllib.parse.quote(name)}")
    except urllib.error.HTTPError as e:
        if e.code == 409:
            return True
        raise
    return False


def _claim_name(pid: str, requested: str) -> str:
    """Store a name for scratch folder pid. A name in use gets the first
    free numbered form: name_2, name_3, and so on."""
    name = requested
    n = 2
    while _name_in_use(name):
        if n > MAX_NAME_NUMBER:
            sys.exit(f'Error: no free name for "{requested}"')
        name = f"{requested}_{n}"
        n += 1
    _fetch("POST", "/memories/store", {"project_id": ALIAS_PROJECT,
                                       "keypath": f"aliases.{pid}",
                                       "content": name,
                                       "category": "config",
                                       "source": "memstate skill scripts"})
    return name


def write_project(project: Optional[str], project_name: Optional[str]) -> str:
    """Project of a set or remember call. In a scratch folder without a
    name, the call must name the project with --project-name."""
    pid = derived_project()
    if not _is_scratch(pid):
        if project_name is not None:
            sys.exit("Error: --project-name is only for a scratch folder")
        return project or pid
    name = session_name()
    if project_name is not None:
        if name and project_name != name:
            sys.exit(f'Error: this folder already has the name "{name}"; '
                     "omit --project-name")
        if not name:
            if not valid_project_name(project_name):
                sys.exit("Error: " + NAME_RULES)
            name = _claim_name(pid, project_name)
    if project:
        return project
    if not name:
        sys.exit("Error: " + NAMING_STEPS)
    return name


def post(path: str, body: dict) -> int:
    return _request("POST", path, body)


def get(path: str) -> int:
    return _request("GET", path, None)
