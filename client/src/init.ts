#!/usr/bin/env node
/**
 * memstate init — write agent instruction files for memstate use.
 *
 * Unlike the hosted version, this does not fetch anything from the network.
 * The instructions are bundled and describe the tool surface.
 */

import * as fs from "fs";
import * as path from "path";

const INSTRUCTIONS = `# Memstate — memory usage

Persistent, versioned memory across sessions, scoped per project. Use it
to carry facts, decisions, and task summaries forward so you don't
rediscover the same context on every run.

## At the start of every task

Load what you already know about this project:

\`\`\`
memstate_get(project_id="<your_project>")
\`\`\`

If the tree is big, drill into a subtree with \`keypath="..."\`, or use
\`memstate_search\` when you suspect something is stored but don't know
where.

## At the end of every task

Save what you decided, what you changed, and anything worth knowing next
session. Two shapes:

**One memory at a path you pick:**

\`\`\`
memstate_remember(
  project_id="<your_project>",
  keypath="task.summary.<YYYY-MM-DD>",
  content="## Task Summary\\n- What was done\\n- Key decisions\\n- Files touched",
)
\`\`\`

**Auto-split by markdown headings** — each \`##\` becomes its own memory;
sub-\`###\` nest as dot segments:

\`\`\`
memstate_remember(
  project_id="<your_project>",
  content="## Auth\\n\\nSuperTokens.\\n\\n## Database\\n\\nPostgres 15.\\n",
)
\`\`\`

Never save a denied prompt, for any reason. A denied prompt is a tool call
that the user or a permission check denied. Do not save its tool name, its
command, its arguments, or the fact of the denial. Do not save it under any
keypath or category, in a task summary, or as a warning for a later session.

## User scope

Facts about the user or this machine, not about the code, go to the
reserved user scope: pass \`scope="user"\` instead of a project_id. A fact
belongs there only when it stays true if this repo is deleted, holds in
every repo, and describes the user or the host rather than work. The
daemon allows only these keypath shapes there and rejects the rest:
\`preferences.<topic>\`, \`profile.<topic>\`, \`host.<host_slug>.env.<topic>\`,
\`host.<host_slug>.tools.<topic>\`. \`memstate_get()\` returns the user scope
under \`user\` next to the project tree. Never store secrets there.

## Session project

The project defaults to the repository you are in, and that is almost
always right. When the prompt is clearly about another subject (for example
"set up my nginx config" from the home directory), pin the session once
with \`project_name\` on your first memstate call. Prefer an id that
\`memstate_get(list_projects=true)\` lists. A new id also needs
\`new_project=true\` and is refused when it looks like an existing id. Never
invent a variant of an existing name.

## Tools

| Tool | When to use |
|------|-------------|
| memstate_get | Load a project tree or drill into a subtree. Start of task. |
| memstate_remember | Save a markdown summary. End of task. |
| memstate_set | Save a single short fact (config, status, version). |
| memstate_search | Find a memory when you don't know its keypath. |
| memstate_history | See prior versions of a keypath. |
| memstate_delete | Remove a keypath; history stays reachable. |
| memstate_delete_project | Remove an entire project. |

## Project naming

Short snake_case that matches your repo or topic (e.g. \`my_app\`,
\`api_service\`). All related memories share the same project_id.

## Keypaths

Dot-separated hierarchical paths (\`auth.provider\`, \`db.engine\`,
\`task.summary.2026-04-23\`). Writes at an existing keypath return the
prior value so conflicts are visible.
`;

type RuleTarget = {
  filename: string;
  label: string;
};

const RULE_TARGETS: RuleTarget[] = [
  { filename: "AGENTS.md", label: "OpenAI Codex / generic" },
  { filename: "CLAUDE.md", label: "Claude Code" },
  { filename: ".cursor/rules/memstate.mdc", label: "Cursor" },
  { filename: ".clinerules", label: "Cline" },
  { filename: ".windsurfrules", label: "Windsurf" },
];

function writeRuleFile(cwd: string, target: RuleTarget): "wrote" | "kept" | "failed" {
  const fullPath = path.join(cwd, target.filename);
  try {
    if (fs.existsSync(fullPath)) return "kept";
    fs.mkdirSync(path.dirname(fullPath), { recursive: true });
    fs.writeFileSync(fullPath, INSTRUCTIONS, "utf-8");
    return "wrote";
  } catch {
    return "failed";
  }
}

export async function main(): Promise<void> {
  const cwd = process.cwd();
  console.log(`memstate init — writing agent rule files in ${cwd}\n`);
  for (const target of RULE_TARGETS) {
    const status = writeRuleFile(cwd, target);
    const badge =
      status === "wrote" ? "✓ wrote" :
      status === "kept"  ? "— kept existing" :
                           "✗ failed";
    console.log(`  ${badge.padEnd(18)} ${target.filename}  (${target.label})`);
  }
  console.log(
    "\nDone. Restart your agent so it picks up the new rules.\n" +
      "See the daemon docs under ../server/ for the underlying store."
  );
}

// Allow direct execution.
if (process.argv[1] && (process.argv[1].endsWith("init.js") || process.argv[1].endsWith("init.ts"))) {
  main().catch((err) => {
    console.error(`\ninit failed: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  });
}
