#!/usr/bin/env node
/**
 * Regression test for the full stack:
 *
 *   MCP client (this file) --stdio--> dist/index.js --HTTP--> memstated --> SQLite
 *
 * The test spawns the built proxy in child mode with a temporary database,
 * calls each MCP tool, and checks the response contracts: write actions
 * (created / superseded / unchanged), heading extraction, tree and keypath
 * reads, FTS search and filters, history with tombstones, recursive delete,
 * project soft-delete and revival, error paths, and the `memstated recall`
 * hook against a second daemon in shared mode (found through daemon.addr).
 * It also checks that the MCP instructions and the `init` rule files tell
 * the agent to never save a denied prompt.
 *
 * The test is hermetic: MEMSTATE_OLLAMA_URL points at a closed port, so
 * embedding is unreachable. Writes must still succeed (fire-and-forget) and
 * semantic search must fail fast — both are asserted below.
 *
 * Run: node client/test/regression.mjs   (after `make build`)
 */
import { execFileSync, spawn } from "child_process";
import * as fs from "fs";
import * as http from "http";
import * as os from "os";
import * as path from "path";
import { fileURLToPath } from "url";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY = path.resolve(__dirname, "..", "dist", "index.js");
const PROJECT = "regress_test";
const DENIED_RULE = "Never save a denied prompt";
const DAEMON =
  process.env.MEMSTATE_BIN ||
  path.resolve(__dirname, "..", "..", "server",
    "memstated" + (process.platform === "win32" ? ".exe" : ""));

let failures = 0;
function check(name, cond, detail = "") {
  if (cond) {
    process.stdout.write(`  ok  ${name}\n`);
  } else {
    failures++;
    process.stdout.write(`FAIL  ${name}${detail ? ` — ${detail}` : ""}\n`);
  }
}

// call invokes one MCP tool and parses the JSON payload the proxy embeds in
// content[0].text. Errors come back as { isError, message }.
async function call(client, name, args) {
  const res = await client.callTool({ name, arguments: args });
  const text = res.content?.[0]?.text ?? "";
  if (res.isError) return { isError: true, message: text };
  return { isError: false, data: JSON.parse(text) };
}

async function main() {
  if (!fs.existsSync(PROXY)) {
    throw new Error(`${PROXY} not found — run \`make build\` first`);
  }
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "memstate-regress-"));
  const watchdog = setTimeout(() => {
    process.stderr.write("regression test timed out after 120s\n");
    process.exit(1);
  }, 120_000);

  const env = { ...process.env };
  delete env.MEMSTATE_ADDR; // force child mode
  env.MEMSTATE_DB = path.join(tmp, "regress.db");
  env.MEMSTATE_NO_UPDATE_CHECK = "1";
  env.MEMSTATE_OLLAMA_URL = "http://127.0.0.1:9"; // closed port: no network

  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [PROXY],
    env,
    stderr: "ignore",
  });
  const client = new Client({ name: "regression-test", version: "0.0.0" });
  await client.connect(transport);

  try {
    // ---- tool surface ---------------------------------------------------
    const { tools } = await client.listTools();
    const names = tools.map((t) => t.name).sort();
    const expected = [
      "memstate_delete",
      "memstate_delete_project",
      "memstate_get",
      "memstate_history",
      "memstate_remember",
      "memstate_search",
      "memstate_set",
    ];
    check(
      "tools/list exposes exactly the 7 memstate tools",
      JSON.stringify(names) === JSON.stringify(expected),
      `got ${names.join(", ")}`
    );

    // ---- denied prompts ---------------------------------------------------
    // Each text that tells the agent to save must also forbid saving a
    // denied prompt.
    check("instructions: forbid saving a denied prompt",
      (client.getInstructions() ?? "").includes(DENIED_RULE),
      "");

    const initDir = path.join(tmp, "init");
    fs.mkdirSync(initDir);
    execFileSync(process.execPath, [PROXY, "init"], { cwd: initDir, stdio: "ignore" });
    check("init: rule files forbid saving a denied prompt",
      fs.readFileSync(path.join(initDir, "CLAUDE.md"), "utf-8").includes(DENIED_RULE),
      "");

    // ---- versioned writes ------------------------------------------------
    let r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "first value with zanzibar token",
      category: "config",
      topics: ["regress"],
    });
    check("set: first write is created v1",
      !r.isError && r.data.action === "created" && r.data.stored.version === 1,
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "second value",
    });
    check("set: rewrite supersedes and returns prior version",
      !r.isError &&
        r.data.action === "superseded" &&
        r.data.stored.version === 2 &&
        r.data.superseded?.content === "first value with zanzibar token",
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "second value",
    });
    check("set: identical rewrite is unchanged (no new version)",
      !r.isError && r.data.action === "unchanged" && r.data.stored.version === 2,
      JSON.stringify(r));

    // ---- remember: explicit keypath and heading extraction ---------------
    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      keypath: "notes.explicit",
      content: "one explicit memory",
    });
    check("remember: explicit keypath stores one item",
      !r.isError && r.data.method === "explicit" && r.data.items.length === 1 &&
        r.data.items[0].keypath === "notes.explicit",
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content:
        "intro prose before headings\n\n" +
        "## Decisions\nuse sqlite\n\n" +
        "### Auth\ntokens only\n\n" +
        "## TODOs\nship it\n",
      category: "note",
    });
    const kps = r.isError ? [] : r.data.items.map((i) => i.keypath).sort();
    check("remember: heading extraction maps sections and aliases",
      !r.isError &&
        r.data.method === "headings" &&
        JSON.stringify(kps) ===
          JSON.stringify(["decisions", "decisions.auth", "preamble", "todo"]),
      JSON.stringify(kps));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content: "prose with no headings at all",
    });
    check("remember: headingless prose lands at preamble",
      !r.isError &&
        r.data.items.length === 1 &&
        r.data.items[0].keypath === "preamble",
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      content: "\n\n",
    });
    check("remember: whitespace-only content with no keypath is an error",
      r.isError && r.message.includes("headings"),
      JSON.stringify(r));

    r = await call(client, "memstate_remember", {
      project_id: PROJECT,
      keypath: "notes.explicit",
      content: "one explicit memory",
      bogus_field: "daemon must reject unknown fields",
    });
    check("remember: unknown request field is rejected",
      r.isError && r.message.includes("bogus_field"),
      JSON.stringify(r));

    // ---- reads ------------------------------------------------------------
    r = await call(client, "memstate_get", { project_id: PROJECT });
    const domains = r.isError ? [] : r.data.domains.map((d) => d.name).sort();
    check("get: tree lists top-level domains and counts memories",
      !r.isError &&
        JSON.stringify(domains) ===
          JSON.stringify(["config", "decisions", "notes", "preamble", "todo"]) &&
        r.data.total_memories === 6,
      JSON.stringify({ domains, total: r.data?.total_memories }));

    r = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config",
    });
    check("get: keypath read returns content",
      !r.isError &&
        r.data.memories.length === 1 &&
        r.data.memories[0].content === "second value",
      JSON.stringify(r));

    r = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config",
      include_content: false,
    });
    check("get: include_content=false strips content",
      !r.isError && r.data.memories[0].content === "",
      JSON.stringify(r));

    // ---- search -----------------------------------------------------------
    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
    });
    check("search: default mode is hybrid and degrades to fts without Ollama",
      !r.isError &&
        r.data.mode === "hybrid" &&
        typeof r.data.degraded === "string" &&
        r.data.results.some((h) => h.keypath === "decisions" &&
          h.sources.length === 1 && h.sources[0] === "fts"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      mode: "fts",
    });
    check("search: fts finds current content",
      !r.isError &&
        r.data.mode === "fts" &&
        r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite zzzznonsense",
      mode: "fts",
    });
    check("search: fts requires every word",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite zzzznonsense",
      mode: "hybrid",
    });
    check("search: hybrid matches on any word",
      !r.isError && r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "zanzibar",
    });
    check("search: superseded content is not indexed",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));
    check("search: zero hits is an empty array, never null",
      !r.isError && Array.isArray(r.data.results) && r.data.results.length === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      category: "note",
    });
    check("search: category filter matches",
      !r.isError && r.data.results.some((h) => h.keypath === "decisions"),
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      category: "gotcha",
    });
    check("search: category filter excludes other categories",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      keypath_prefix: "notes",
    });
    check("search: keypath_prefix filter excludes other subtrees",
      !r.isError && r.data.total_found === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_search", {
      project_id: PROJECT,
      query: "sqlite",
      mode: "semantic",
    });
    check("search: semantic fails fast when embedder is unreachable",
      r.isError && r.message.includes("embed query"),
      JSON.stringify(r));

    // ---- history and delete -----------------------------------------------
    r = await call(client, "memstate_history", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("history: two versions after one supersede",
      !r.isError && r.data.total_versions === 2,
      JSON.stringify(r));

    r = await call(client, "memstate_delete", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: single keypath tombstoned",
      !r.isError && r.data.deleted_count === 1,
      JSON.stringify(r));

    r = await call(client, "memstate_get", { project_id: PROJECT, keypath: "config" });
    check("delete: tombstoned keypath gone from reads",
      !r.isError && Array.isArray(r.data.memories) && r.data.memories.length === 0,
      JSON.stringify(r));

    r = await call(client, "memstate_history", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: history keeps all versions plus tombstone",
      !r.isError && r.data.total_versions === 3,
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.alpha",
      value: "revived value",
    });
    const revived = await call(client, "memstate_get", {
      project_id: PROJECT,
      keypath: "config.alpha",
    });
    check("delete: rewrite resurrects a tombstoned keypath",
      !r.isError &&
        !revived.isError &&
        revived.data.memories[0]?.content === "revived value",
      JSON.stringify(revived));

    // ---- recursive delete ---------------------------------------------------
    await call(client, "memstate_set", {
      project_id: PROJECT, keypath: "branches.tmp.todo", value: "a",
    });
    await call(client, "memstate_set", {
      project_id: PROJECT, keypath: "branches.tmp.notes", value: "b",
    });
    r = await call(client, "memstate_delete", {
      project_id: PROJECT,
      keypath: "branches.tmp",
      recursive: true,
    });
    check("delete: recursive removes the whole subtree",
      !r.isError &&
        r.data.deleted_count === 2 &&
        r.data.deleted_keypaths.sort().join(",") ===
          "branches.tmp.notes,branches.tmp.todo",
      JSON.stringify(r));

    // ---- project soft-delete and revival ------------------------------------
    r = await call(client, "memstate_delete_project", { project_id: PROJECT });
    check("delete_project: reports deleted memory count",
      !r.isError && r.data.deleted_count > 0,
      JSON.stringify(r));

    r = await call(client, "memstate_get", { project_id: PROJECT });
    check("delete_project: reads on a soft-deleted project fail",
      r.isError && r.message.includes("soft-deleted"),
      JSON.stringify(r));

    r = await call(client, "memstate_set", {
      project_id: PROJECT,
      keypath: "config.beta",
      value: "revive project",
    });
    const tree = await call(client, "memstate_get", { project_id: PROJECT });
    check("delete_project: any write revives the project with memories intact",
      !r.isError && !tree.isError && tree.data.total_memories > 1,
      JSON.stringify(tree));

    // ---- user scope -----------------------------------------------------------
    // The reserved project `_user` holds facts about the user and this
    // machine. The daemon allows only preferences.*, profile.*,
    // host.<slug>.env.* and host.<slug>.tools.* there.
    const instr = client.getInstructions() ?? "";
    const hostSlug = /host slug is "([a-z0-9_]+)"/.exec(instr)?.[1] ?? "";
    check("instructions: describe the user scope and this host's slug",
      instr.includes("User scope") && hostSlug !== "",
      "");
    r = await call(client, "memstate_set", {
      scope: "user", keypath: "preferences.commit_style",
      value: "short subjects, no trailers",
    });
    check("user scope: preferences write is accepted",
      !r.isError && r.data.action === "created",
      JSON.stringify(r));
    r = await call(client, "memstate_set", { scope: "user", keypath: "todo.x", value: "nope" });
    check("user scope: todo write is rejected with the allowed shapes",
      r.isError && r.message.includes("allowed"),
      JSON.stringify(r));
    r = await call(client, "memstate_set", { project_id: "_scratch", keypath: "preferences.x", value: "nope" });
    check("user scope: other reserved ids are rejected",
      r.isError && r.message.includes("reserved"),
      JSON.stringify(r));
    r = await call(client, "memstate_set", {
      scope: "user", project_id: PROJECT, keypath: "preferences.x", value: "nope",
    });
    check("user scope: scope and project_id together is an error",
      r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_set", {
      scope: "user", keypath: `host.${hostSlug}.env.go_bin`,
      value: "go binaries live in ~/.go/bin",
    });
    check("user scope: this host's env write is accepted",
      !r.isError && r.data.action === "created",
      JSON.stringify(r));
    r = await call(client, "memstate_set", {
      project_id: "_user", keypath: "host.other_box.env.go_bin",
      value: "go binaries live in /opt/go/bin on the other box",
    });
    check("user scope: another host's env write is accepted",
      !r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_id: PROJECT });
    const hostNode = r.data?.user?.domains?.find((d) => d.name === "host");
    check("get: tree carries the user scope pruned to this host",
      !r.isError && r.data.user.host === hostSlug &&
        r.data.user.domains.some((d) => d.name === "preferences") &&
        hostNode && hostNode.children.length === 1 &&
        hostNode.children[0].name === hostSlug &&
        r.data.user.total_memories === 2,
      JSON.stringify(r.data?.user));
    r = await call(client, "memstate_search", { scope: "user", query: "go binaries live", mode: "fts" });
    check("search: user scope drops other hosts",
      !r.isError && r.data.results.length === 1 &&
        r.data.results[0].keypath === `host.${hostSlug}.env.go_bin`,
      JSON.stringify(r));
    r = await call(client, "memstate_remember", {
      scope: "user", content: "## Preferences\nanswer tersely\n\n## Todo\nfinish\n",
    });
    check("remember: user scope rejects a summary with a todo section",
      r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_get", { scope: "user", keypath: "preferences" });
    check("remember: the rejected summary wrote nothing",
      !r.isError && r.data.memories.length === 1 &&
        r.data.memories[0].content.includes("no trailers"),
      JSON.stringify(r));
    // A soft-deleted _user project must not break every project read.
    r = await call(client, "memstate_delete_project", { project_id: "_user" });
    check("user scope: the reserved project can be soft-deleted",
      !r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_id: PROJECT });
    check("get: a deleted user scope yields an empty user block, not an error",
      !r.isError && r.data.domains.length > 0 && r.data.user.total_memories === 0,
      JSON.stringify(r));
    r = await call(client, "memstate_set", {
      scope: "user", keypath: "preferences.commit_style",
      value: "short subjects, no trailers, imperative mood",
    });
    const userTree = await call(client, "memstate_get", { scope: "user" });
    check("user scope: a write revives the reserved project",
      !r.isError && r.data.action === "superseded" &&
        !userTree.isError && userTree.data.total_memories >= 1,
      JSON.stringify([r, userTree]));

    // ---- session project ------------------------------------------------------
    // The cwd project stays the default. project_name pins a session once,
    // with caution rules against misspelled or invented names.
    check("instructions: describe the session project override",
      instr.includes("Session project") && instr.includes("new_project"),
      "");
    r = await call(client, "memstate_set", { project_id: "other_proj", keypath: "notes.a", value: "seed" });
    check("session: seed another project", !r.isError, JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "nope_project" });
    check("session: an unknown project_name needs new_project",
      r.isError && r.message.includes("new_project") && r.message.includes("list_projects"),
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "regress_tests", new_project: true });
    check("session: a near-duplicate name is refused even with new_project",
      r.isError && r.message.includes(`"${PROJECT}"`),
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "otherproj", new_project: true });
    check("session: a name equal after normalization is refused",
      r.isError && r.message.includes('"other_proj"'),
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "_user" });
    check("session: a reserved or malformed name is refused",
      r.isError && r.message.includes("snake_case"),
      JSON.stringify(r));
    r = await call(client, "memstate_set", {
      project_name: "other_proj", scope: "user", keypath: "preferences.x", value: "v",
    });
    check("session: project_name with scope=user is an error",
      r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "taxes_2026", new_project: true });
    check("session: a clearly new name with new_project pins the session",
      !r.isError && r.data.session_project === "taxes_2026" && r.data.project_id === "taxes_2026",
      JSON.stringify(r));
    r = await call(client, "memstate_set", { keypath: "notes.b", value: "pinned write" });
    check("session: later calls without project_id use the pinned project",
      !r.isError && r.data.stored.project_id === "taxes_2026" && r.data.session_project === "taxes_2026",
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "other_proj" });
    check("session: a second different project_name is an error",
      r.isError && r.message.includes("pinned"),
      JSON.stringify(r));
    r = await call(client, "memstate_get", { project_name: "taxes_2026" });
    check("session: the same project_name again is fine",
      !r.isError,
      JSON.stringify(r));
    r = await call(client, "memstate_set", { project_id: PROJECT, keypath: "config.gamma", value: "explicit id beats the pin" });
    check("session: explicit project_id still reaches another project",
      !r.isError && r.data.stored.project_id === PROJECT,
      JSON.stringify(r));
    const projects = await call(client, "memstate_get", { list_projects: true });
    const ids = projects.data.projects.map((p) => p.id);
    check("session: refused names created no project",
      !ids.includes("nope_project") && !ids.includes("regress_tests") && !ids.includes("otherproj") &&
        ids.includes("taxes_2026"),
      ids.join(","));
    // A fresh proxy has no pin; an existing id pins it directly. Its cwd
    // basename slugs to PROJECT.
    const freshCwd = path.join(tmp, "regress_test");
    fs.mkdirSync(freshCwd);
    await withProxy(env, freshCwd, async (fresh) => {
      let f = await call(fresh, "memstate_get", {});
      check("session: a new proxy starts on the cwd default without a pin",
        !f.isError && f.data.project_id === PROJECT && f.data.session_project === undefined,
        JSON.stringify(f));
      f = await call(fresh, "memstate_get", { project_name: "other_proj" });
      check("session: an existing project_name pins without new_project",
        !f.isError && f.data.session_project === "other_proj" && f.data.project_id === "other_proj",
        JSON.stringify(f));
      f = await call(fresh, "memstate_search", { query: "seed", mode: "fts" });
      check("session: search without project_id uses the pin",
        !f.isError && f.data.results.length === 1 && f.data.results[0].project_id === "other_proj",
        JSON.stringify(f));
    });

    // ---- project creation gate ------------------------------------------------
    // Outside a git repository the directory name is not a project. A write
    // to the cwd default is refused until that project exists, unless the
    // call carries new_project=true. Reads, explicit project_id and a pinned
    // session are free. The home directory never gets a default for writes.
    const gateCwd = path.join(tmp, "gate_dir");
    fs.mkdirSync(gateCwd);
    await withProxy(env, gateCwd, async (g) => {
      let f = await call(g, "memstate_get", {});
      check("gate: a read from a non-repo dir with no project is free",
        !f.isError && f.data.project_id === "gate_dir" && f.data.total_memories === 0,
        JSON.stringify(f));
      f = await call(g, "memstate_set", { keypath: "notes.a", value: "v" });
      check("gate: a write that would create the cwd project is refused",
        f.isError && f.message.includes("new_project") && f.message.includes('"gate_dir"'),
        JSON.stringify(f));
      f = await call(g, "memstate_remember", { content: "## Notes\nbody\n" });
      check("gate: remember is gated like set",
        f.isError && f.message.includes("new_project"),
        JSON.stringify(f));
      f = await call(g, "memstate_set", { project_id: PROJECT, keypath: "config.delta", value: "explicit" });
      check("gate: explicit project_id bypasses the gate",
        !f.isError && f.data.stored.project_id === PROJECT,
        JSON.stringify(f));
      f = await call(g, "memstate_set", { keypath: "notes.a", value: "v", new_project: true });
      check("gate: new_project=true creates the cwd project",
        !f.isError && f.data.stored.project_id === "gate_dir",
        JSON.stringify(f));
      f = await call(g, "memstate_set", { keypath: "notes.b", value: "w" });
      check("gate: once the project exists, writes need no flag",
        !f.isError && f.data.stored.project_id === "gate_dir",
        JSON.stringify(f));
    });
    // The cwd basename resembles an existing project: refused, the existing
    // id named, and new_project does not override that.
    const nearCwd = path.join(tmp, "regress_tests");
    fs.mkdirSync(nearCwd);
    await withProxy(env, nearCwd, async (n) => {
      let f = await call(n, "memstate_set", { keypath: "notes.a", value: "v", new_project: true });
      check("gate: a cwd that looks like an existing project is refused",
        f.isError && f.message.includes(`"${PROJECT}"`) && f.message.includes("project_name"),
        JSON.stringify(f));
      f = await call(n, "memstate_set", { project_name: PROJECT, keypath: "notes.near", value: "pinned" });
      check("gate: pinning the existing project is the way out",
        !f.isError && f.data.stored.project_id === PROJECT && f.data.session_project === PROJECT,
        JSON.stringify(f));
    });
    // Skipped when the home directory itself is a git repository.
    let homeIsRepo = true;
    try {
      execFileSync("git", ["-C", os.homedir(), "rev-parse", "--show-toplevel"], { stdio: "ignore" });
    } catch {
      homeIsRepo = false;
    }
    if (!homeIsRepo) {
      await withProxy(env, os.homedir(), async (h) => {
        let f = await call(h, "memstate_set", { keypath: "notes.a", value: "v", new_project: true });
        check("gate: a write from the home directory is refused even with new_project",
          f.isError && f.message.includes("home directory") && f.message.includes("project_name") &&
            f.message.includes('scope="user"'),
          JSON.stringify(f));
        f = await call(h, "memstate_set", { scope: "user", keypath: "preferences.gate", value: "v" });
        check("gate: the user scope is open from the home directory",
          !f.isError && f.data.stored.project_id === "_user",
          JSON.stringify(f));
        f = await call(h, "memstate_set", { project_name: "other_proj", keypath: "notes.home", value: "v" });
        check("gate: a pin from the home directory writes to the pinned project",
          !f.isError && f.data.stored.project_id === "other_proj" && f.data.session_project === "other_proj",
          JSON.stringify(f));
      });
    }
    const after = await call(client, "memstate_get", { list_projects: true });
    const afterIds = after.data.projects.map((p) => p.id);
    const homeId = path.basename(os.homedir()).toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "");
    check("gate: refused writes created no project",
      afterIds.includes("gate_dir") && !afterIds.includes("regress_tests") &&
        (homeIsRepo || !afterIds.includes(homeId)),
      afterIds.join(","));

    // ---- error path -----------------------------------------------------------
    const bad = await client.callTool({ name: "memstate_set", arguments: {} });
    check("set: missing required fields is an error",
      bad.isError === true,
      JSON.stringify(bad));

    // ---- recall hook ----------------------------------------------------------
    // A shared-mode daemon on the same DB publishes daemon.addr next to it;
    // `memstated recall` has no MEMSTATE_ADDR here, so it must discover the
    // daemon through that file. The cwd basename slugs to PROJECT.
    await withSharedDaemon(env, async () => {
      const cwd = path.join(tmp, "regress-test");
      fs.mkdirSync(cwd);
      const recall = (session) =>
        execFileSync(DAEMON, ["recall"], {
          env,
          input: JSON.stringify({
            session_id: session,
            cwd,
            prompt: "why did we choose sqlite for the store",
          }),
        }).toString();
      const first = recall("regress_s1");
      check("recall: finds the shared daemon via daemon.addr and injects a hit",
        first.includes(`<memstate-recall project="${PROJECT}">`) &&
          first.includes("### decisions"),
        JSON.stringify(first));
      check("recall: the scope block says whether the cwd project exists",
        first.includes(`<memstate-scope cwd_project="${PROJECT}" exists="true">`),
        JSON.stringify(first));
      check("recall: same session does not repeat a keypath",
        recall("regress_s1") === "",
        JSON.stringify(first));
      check("recall: a new session sees the keypath again",
        recall("regress_s2").includes("### decisions"),
        "");
    });
  } finally {
    clearTimeout(watchdog);
    await client.close();
    fs.rmSync(tmp, { recursive: true, force: true });
  }

  await attachToSlowDaemon();
  await attachRetriesHealth();
  await noDaemonForRemoteAddr();

  if (failures > 0) {
    process.stdout.write(`\n${failures} regression check(s) FAILED\n`);
    process.exit(1);
  }
  process.stdout.write("\nall regression checks passed\n");
}

// withProxy connects a second, fresh proxy (own process, own session pin)
// to the same DB in child mode, runs fn, and closes it.
async function withProxy(env, cwd, fn) {
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [PROXY],
    env,
    cwd,
    stderr: "ignore",
  });
  const fresh = new Client({ name: "regression-test-2", version: "0.0.0" });
  await fresh.connect(transport);
  try {
    await fn(fresh);
  } finally {
    await fresh.close();
  }
}

// withSharedDaemon starts `memstated --addr 127.0.0.1:0` with env, waits
// for its READY banner, runs fn, then stops it and waits for exit.
async function withSharedDaemon(env, fn) {
  const child = spawn(DAEMON, ["--addr", "127.0.0.1:0"], {
    env,
    stdio: ["ignore", "ignore", "pipe"],
  });
  const addr = await new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => reject(new Error("shared daemon: no READY banner")), 5000);
    child.stderr.on("data", (chunk) => {
      buf += chunk.toString();
      const m = /MEMSTATE_READY addr=(\S+)/.exec(buf);
      if (m) {
        clearTimeout(timer);
        child.stderr.removeAllListeners("data");
        child.stderr.resume();
        resolve(m[1]);
      }
    });
    child.on("exit", (code) => reject(new Error(`shared daemon exited early (${code})`)));
  });
  const exited = new Promise((resolve) => child.on("exit", resolve));
  try {
    await fn(addr);
  } finally {
    execFileSync(DAEMON, ["stop", "--addr", addr], { env, stdio: "ignore" });
    await Promise.race([exited, new Promise((r) => setTimeout(r, 5000))]);
    if (child.exitCode === null) child.kill("SIGKILL");
  }
}

// attachToSlowDaemon: a daemon on another machine answers /health late,
// because a new connection costs two round trips. On 2026-09-25 a daemon at
// 10.66.0.1 over WireGuard (280 ms round trip) took 680 ms, the proxy gave up
// after 500 ms, and it tried to start its own daemon on an address that was
// not local. The proxy must wait for the daemon and attach to it. A proxy that
// starts a daemon writes the daemon log under $HOME, so an empty $HOME shows
// that none was started.
async function attachToSlowDaemon() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "memstate-attach-"));
  const fake = http.createServer((req, res) => {
    setTimeout(() => {
      if (req.url === "/health") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ service: "memstate", version: "test", embed_model: "" }));
      } else {
        res.writeHead(404);
        res.end();
      }
    }, 800);
  });
  await new Promise((resolve) => fake.listen(0, "127.0.0.1", resolve));
  const addr = `127.0.0.1:${fake.address().port}`;
  try {
    const out = await runProxy(addr, home);
    check("attach: a daemon that answers /health slowly is attached",
      out.includes("mode=attach"),
      out.trim());
    check("attach: no second daemon is started for it",
      !fs.existsSync(path.join(home, ".memstate", "memstated.log")),
      "the proxy started a daemon");
  } finally {
    fake.close();
    fs.rmSync(home, { recursive: true, force: true });
  }
}

// attachRetriesHealth: on 2026-09-26 the VPS behind 10.66.0.1 dropped most new
// TCP connections, because its conntrack table was full. One /health probe
// timed out, and the proxy started its own daemon at once. The proxy must probe
// again before it gives up. The fake daemon holds the first /health request
// past the probe timeout, and answers the next request at once.
async function attachRetriesHealth() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "memstate-retry-"));
  let requests = 0;
  const fake = http.createServer((req, res) => {
    requests++;
    setTimeout(() => {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ service: "memstate", version: "test", embed_model: "" }));
    }, requests === 1 ? 4000 : 0);
  });
  await new Promise((resolve) => fake.listen(0, "127.0.0.1", resolve));
  const addr = `127.0.0.1:${fake.address().port}`;
  try {
    const out = await runProxy(addr, home);
    check("retry: a daemon that misses one /health probe is attached",
      out.includes("mode=attach"),
      out.trim());
    check("retry: no second daemon is started for it",
      !fs.existsSync(path.join(home, ".memstate", "memstated.log")),
      "the proxy started a daemon");
  } finally {
    fake.closeAllConnections();
    fake.close();
    fs.rmSync(home, { recursive: true, force: true });
  }
}

// noDaemonForRemoteAddr: a daemon cannot listen on an address of another
// machine. On 2026-09-26 the proxy tried to start one on 10.66.0.1:8767 and
// the daemon failed with "bind: The requested address is not valid in its
// context". When nothing answers at an IP address that this machine does not
// have, the proxy must fail with a clear message and start no daemon.
// 192.0.2.1 is in TEST-NET-1 (RFC 5737), so no machine has it.
async function noDaemonForRemoteAddr() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "memstate-remote-"));
  try {
    const out = await runProxy("192.0.2.1:8767", home);
    check("remote: the proxy says that it does not start a daemon there",
      out.includes("not an address of this machine"),
      out.trim());
    check("remote: no daemon is started for an address of another machine",
      !fs.existsSync(path.join(home, ".memstate", "memstated.log")),
      "the proxy started a daemon");
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
}

// runProxy runs the proxy smoke test (`--test`) with MEMSTATE_ADDR=addr and
// HOME=home, and returns its stdout and stderr. The exit code is not checked:
// on Windows, Node can abort in process.exit while a fetch socket closes
// (libuv assertion in src\win\async.c), and the smoke test `--test` shows the
// same with any daemon.
async function runProxy(addr, home) {
  const env = { ...process.env, MEMSTATE_ADDR: addr, HOME: home, MEMSTATE_NO_UPDATE_CHECK: "1" };
  const proxy = spawn(process.execPath, [PROXY, "--test"], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "";
  proxy.stdout.on("data", (d) => (out += d));
  proxy.stderr.on("data", (d) => (out += d));
  await new Promise((resolve) => proxy.on("exit", resolve));
  return out;
}

main().catch((err) => {
  process.stderr.write(`regression test error: ${err?.stack ?? err}\n`);
  process.exit(1);
});
