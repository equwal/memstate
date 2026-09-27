#!/usr/bin/env python3
"""Browse and retrieve memories from the memstated daemon.

Usage:
  memstate_get.py --list-projects                    # List all project ids
  memstate_get.py --project my_app                   # Project tree plus the user scope (names only)
  memstate_get.py --project my_app --keypath db --include-content  # Subtree with content
  memstate_get.py --scope user                       # The user scope tree, pruned to this host
  memstate_get.py --scope user --keypath preferences # User-scope subtree with content
  memstate_get.py --memory-id 42                     # Single memory by numeric ID
"""
import argparse
import sys
import urllib.error
import urllib.parse

from _client import (USER_PROJECT, add_scope_args, count_values, emit, fetch,
                     get, host_slug, prune_other_hosts, resolve_project)


def _tree(project: str) -> dict:
    return fetch("GET", f"/tree?project_id={urllib.parse.quote(project)}")


def _user_tree() -> dict:
    """The user scope, pruned to this machine's host.<slug> subtree. A
    soft-deleted _user project (HTTP 409) yields an empty scope so the
    project tree still prints."""
    try:
        domains = _tree(USER_PROJECT).get("domains") or []
    except urllib.error.HTTPError:
        domains = []
    domains = prune_other_hosts(domains)
    return {"host": host_slug(), "domains": domains, "total_memories": count_values(domains)}


def main() -> int:
    ap = argparse.ArgumentParser(description="Browse and retrieve memories (server)")
    add_scope_args(ap)
    ap.add_argument("--list-projects", action="store_true",
                    help="list every project id in the store")
    ap.add_argument("--keypath")
    ap.add_argument("--memory-id", type=int)
    ap.add_argument("--include-content", action="store_true")
    args = ap.parse_args()

    if args.list_projects:
        return get("/projects")

    if args.memory_id is not None:
        return get(f"/memories/{args.memory_id}")

    project = resolve_project(args)
    if args.keypath:
        body = {
            "project_id": project,
            "keypath": args.keypath,
            "recursive": True,
            "include_content": args.include_content,
        }
        return emit(lambda: fetch("POST", "/keypaths", body))

    if project == USER_PROJECT:
        return emit(_user_tree)

    # The user scope rides along with every project tree, like the proxy.
    def combined() -> dict:
        tree = _tree(project)
        tree["user"] = _user_tree()
        return tree

    return emit(combined)


if __name__ == "__main__":
    sys.exit(main())
