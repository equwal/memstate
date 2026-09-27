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
import socket
import subprocess
import sys
import threading
import time
import urllib.error
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


def fetch(method: str, path: str, body: Optional[dict] = None):
    """One daemon call. Returns the parsed JSON body (or the raw text when
    it is not JSON). Raises urllib errors; use emit() to turn them into an
    exit code."""
    url = f"{_base()}{path}"
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=_HEADERS, method=method)
    with urllib.request.urlopen(req) as resp:
        payload = resp.read().decode("utf-8")
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            return payload


def emit(call) -> int:
    """Run call(), print its result as JSON, and map daemon errors to exit
    codes: 1 for an HTTP error, 2 for an unreachable daemon."""
    try:
        result = call()
        if isinstance(result, str):
            print(result)
        else:
            print(json.dumps(result, indent=2))
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


def default_project() -> str:
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
    return slug_name(base)


def slug_name(name: str) -> str:
    """Shared id rule: lowercase, runs of other characters become "_",
    edge underscores trimmed. Same rule as the TS proxy and the Go daemon."""
    slug = re.sub(r"[^a-z0-9]+", "_", name.lower()).strip("_")
    return slug or "default"


# USER_PROJECT is the daemon's one reserved project for facts about the
# user and the host. The daemon rejects writes there outside a short
# allowlist of keypath shapes (see SKILL.md, "User scope").
USER_PROJECT = "_user"


def host_slug() -> str:
    """This machine's segment under host.<slug> in USER_PROJECT: the first
    hostname label, slugged. Same rule as the TS proxy and the Go daemon."""
    return slug_name(socket.gethostname().split(".")[0])


def add_scope_args(ap) -> None:
    """Add the --project / --scope pair every script accepts."""
    ap.add_argument("--project", default=None,
                    help="project id (default: derived from repo/dir name)")
    ap.add_argument("--scope", choices=("project", "user"), default="project",
                    help="'user' targets the reserved user scope (facts about "
                         "the user or this machine, not about this repo)")


def resolve_project(args) -> str:
    """Project id for a call: the reserved user project for --scope user,
    else --project, else this repo's default."""
    if args.scope == "user":
        if args.project:
            raise SystemExit("Error: pass --scope user or --project, not both")
        return USER_PROJECT
    return args.project or default_project()


def is_other_host(keypath: str) -> bool:
    """True when a user-scope keypath describes another machine."""
    seg = keypath.split(".")
    return seg[0] == "host" and len(seg) >= 2 and seg[1] != host_slug()


def prune_other_hosts(domains: list) -> list:
    """Keep only this machine's subtree under `host`."""
    out = []
    for d in domains:
        if d.get("name") == "host":
            d = dict(d)
            d["children"] = [c for c in d.get("children", []) if c.get("name") == host_slug()]
        out.append(d)
    return out


def count_values(nodes: list) -> int:
    n = 0
    for node in nodes:
        if node.get("has_value"):
            n += 1
        n += count_values(node.get("children", []))
    return n


def post(path: str, body: dict) -> int:
    return emit(lambda: fetch("POST", path, body))


def get(path: str) -> int:
    return emit(lambda: fetch("GET", path, None))
