# Migrating from Conductor

Conductor.build (the macOS workspace manager) and canopy use the same configuration shape — three script paths and three env vars — so the migration is mostly mechanical. This guide walks through it for a real Rails project (`cravd`), but the same steps apply to anything that runs Conductor today.

## What stays the same

- Your `bin/conductor-setup`, `bin/conductor-teardown`, etc. — the scripts themselves keep working unchanged.
- Your project layout and dev workflow.
- The schema: three scripts (`setup`, `run`, `archive`) and three env vars passed in.

## What changes

- `conductor.json` -> `canopy.json` (you can keep both side by side; canopy doesn't touch conductor's file).
- `CONDUCTOR_*` env vars -> `CANOPY_*` (one-line `sed` per file).
- macOS GUI -> Linux TUI (canopy command surface).

## Step 1: generate canopy.json

`canopy init` detects an existing `conductor.json` and copies its script paths verbatim:

```bash
cd ~/Work/cravd
canopy init
```

Output looks like:

```
Wrote /home/you/Work/cravd/canopy.json
  (mirrored scripts from /home/you/Work/cravd/conductor.json — canopy uses the same schema)

Next steps:
  1. Review canopy.json and confirm the script paths look right.
  2. If your scripts read CONDUCTOR_PORT / CONDUCTOR_WORKSPACE_PATH /
     CONDUCTOR_ROOT_PATH from env, switch them to the CANOPY_* equivalents.
  3. If config files (database.yml, etc.) reference CONDUCTOR_*, update those too.
  4. Commit canopy.json.
  5. Run `canopy new` to verify.
```

`conductor.json` is left untouched. The new `canopy.json` points at your existing `bin/conductor-*` scripts unchanged.

## Step 2: update env-var references

Anywhere your scripts or config files reference `CONDUCTOR_*`, switch to `CANOPY_*`. The two sets are 1:1 — same meaning, different prefix.

| Conductor | canopy |
|---|---|
| `CONDUCTOR_WORKSPACE_PATH` | `CANOPY_WORKSPACE_PATH` |
| `CONDUCTOR_ROOT_PATH` | `CANOPY_ROOT_PATH` |
| `CONDUCTOR_PORT` | `CANOPY_PORT` |

The mechanical fix is `sed`. From your project root:

```bash
# Update the scripts
sed -i 's/CONDUCTOR_/CANOPY_/g' bin/conductor-*

# Update database.yml or any config files that read CONDUCTOR_PORT
sed -i 's/CONDUCTOR_PORT/CANOPY_PORT/g' config/database.yml
```

Adjust the file list to match your project. Review the diff (`git diff`) before committing.

## Step 3 (optional): rename the scripts

This is purely cosmetic. canopy is happy invoking `bin/conductor-setup` forever. If you want canopy-named scripts:

```bash
git mv bin/conductor-setup bin/canopy-setup
git mv bin/conductor-teardown bin/canopy-archive
# update canopy.json's script paths to match
```

Then `git diff canopy.json` should show:

```diff
-    "setup": "bin/conductor-setup",
+    "setup": "bin/canopy-setup",
-    "archive": "bin/conductor-teardown",
+    "archive": "bin/canopy-archive",
```

## Step 4: verify

```bash
canopy new --no-attach
```

Watch the setup output:

```
Basing bold-falcon on origin/main
canopy workspace setup:
  CANOPY_WORKSPACE_PATH = /home/you/.canopy/workspaces/cravd/bold-falcon
  CANOPY_ROOT_PATH      = /home/you/Work/cravd
  CANOPY_PORT           = 40010
bundle install...
bin/rails db:create RAILS_ENV=development
Setup complete.
Workspace ready: bold-falcon
```

Then attach and confirm `bin/dev` works:

```bash
canopy switch bold-falcon
# inside the shell pane:
bin/dev   # should bind to port 40010 via CANOPY_PORT
```

Tear down:

```bash
canopy rm bold-falcon -y
```

## Coexistence with Conductor

Nothing prevents both from running side by side. canopy uses `~/.canopy/` for state, Conductor uses `~/Library/Application Support/Conductor/` (or wherever its store is on macOS). They don't share state, but they CAN share scripts since the env-var mappings are clean.

If you keep `conductor.json` around, you can switch back to Conductor at any time without touching canopy's state. Once you're confident, `git rm conductor.json bin/conductor-*` to clean up.

## See also

- `docs/canopy-json.md` — full schema reference
- `docs/troubleshooting.md` — common migration gotchas
