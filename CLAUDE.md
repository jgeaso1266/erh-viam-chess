# Repo notes for AI assistants

## Don't remove env-var-gated debug logging

Some files contain debug scaffolding that's toggled by an environment variable, e.g.:

```go
var debugFind = os.Getenv("DEBUG_FIND") != ""
```

and guarded `fmt.Fprintf(os.Stderr, ...)` blocks. These are **permanent tooling**, not task-scoped scratch — they're used to inspect detector internals on real images (e.g. `DEBUG_FIND=1 go test -run TestFindBoardCorners/data/board14.jpg`). Leave them in place when finishing a task. Only strip debug code that is unconditional (e.g. always-on `fmt.Println`s added mid-task).

Current debug toggles:
- `DEBUG_FIND` — `board_finder.go`, dumps best hypothesis + aligned-line details from `findBorderPair`.
