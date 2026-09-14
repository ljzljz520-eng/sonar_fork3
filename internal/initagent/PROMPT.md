Write a `sonar.yaml` for this project.

`sonar.yaml` sits at the repository root and says how the project runs, so that
`sonar start` brings it up the way a `dev.sh` would — except that sonar picks
the ports and tells each service where the others are.

## What sonar already knows

- Project root: `{{.Root}}`
- Write the file to: `{{.ConfigPath}}`
{{- if .Legacy}}
- There is an older `{{.Legacy}}`. Move it with `git mv` instead of leaving two files behind.
{{- end}}
{{- if .Listening}}

Listening right now, inside this project:

{{range .Listening}}- {{.}}
{{end}}
{{- end}}

This is the draft sonar writes on its own. It is a starting point, not an
answer: it knows only what is running, plus whatever a compose file or a
`package.json` states outright.

```yaml
{{.Draft}}
```

## The format

```yaml
name: my-app                      # the group name; no slashes or spaces
services:
  - name: db
    cmd: docker compose up db     # run directly, never through a shell
    port: 5432                    # a fixed port the service must have
  - name: api
    cmd: uv run uvicorn app:app --port ${port}
    cwd: backend                  # relative to this file, and inside it
    port: auto                    # sonar picks a free port, the same one each time
    health: /healthz              # an HTTP path sonar polls while it runs
    depends_on: [db]              # started after db is listening
    env:
      DATABASE_URL: postgres://localhost:${db.port}/app
  - name: frontend
    cmd: npm run dev -- --port ${port} --strictPort
    port: auto
    depends_on: [api]
    env:
      VITE_API_URL: ${api.url}
```

`${port}` and `${url}` are this service's own; `${<service>.port}` and
`${<service>.url}` are another's. They work in `cmd` and in `env`. A service
with `port: auto` is given `PORT` and `SONAR_PORT` in its environment as well.

## How to go about it

1. Read enough of the repository to know what someone runs to work on it: the
   README's getting-started section, `dev.sh` or `scripts/dev.sh`, a `Makefile`,
   a `Procfile`, `package.json`, `pyproject.toml`, `Cargo.toml`, `go.mod`,
   `manage.py`, a compose file — whatever this project actually uses.
2. Write one service per long-running process. A build step, a test runner, a
   migration and a linter are not services.
3. Prefer `port: auto` and pass the port into the command, so two checkouts of
   this repository can run side by side. Keep a fixed port only where something
   outside the project expects that exact number.
4. Where a service needs another's address, use `${other.port}` or
   `${other.url}` in `env:` instead of a hard-coded `localhost:8080`.
5. Order with `depends_on` where one service genuinely needs another listening.
6. Write the file, then check it: `sonar groups` lists what the file declares,
   and `sonar doctor` reports a file that does not load.
{{- if .HasSkill}}
7. The `sonar` skill is installed here and documents the rest of the tool.
{{- end}}

## Rules

- Do not start, stop or kill anything. Writing the file is the whole task.
- Do not invent a service. If the repository does not say how something runs,
  leave it out and say so at the end.
- Only put a port in `cmd` if that program takes one there.
- Keep `cmd` runnable without a shell: no `&&`, no pipes, no `cd`. Use `cwd`.
- Leave the file's comments and any existing services alone unless they are
  wrong.

When you are done, show the file and say briefly what you could not work out.
