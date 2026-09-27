#!/usr/bin/env python3
"""Browse and retrieve memories from the memstated daemon.

Usage:
  memstate_get.py --list-projects                    # List all project ids
  memstate_get.py --project my_app                   # Project tree (names only)
  memstate_get.py --project my_app --keypath db --include-content  # Subtree with content
  memstate_get.py --memory-id 42                     # Single memory by numeric ID
"""
import argparse
import sys
import urllib.parse

from _client import require_project, get, post


def main() -> int:
    ap = argparse.ArgumentParser(description="Browse and retrieve memories (server)")
    ap.add_argument("--project", default=None,
                    help="project id (required: the scripts do not derive one)")
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

    project = require_project(args.project)
    if args.keypath:
        body = {
            "project_id": project,
            "keypath": args.keypath,
            "recursive": True,
            "include_content": args.include_content,
        }
        return post("/keypaths", body)

    return get(f"/tree?project_id={urllib.parse.quote(project)}")


if __name__ == "__main__":
    sys.exit(main())
