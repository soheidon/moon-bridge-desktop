# Moon Bridge Desktop

Moon Bridge Desktop is a Wails/Go desktop shell around the existing Go Moon Bridge
gateway. The UI currently includes the DeepSeek routing card and a Windows-only
Codex launcher.

## Local development

From `desktop/`:

```powershell
npm install
npm run build:wails
```

Codex launcher prerequisites:

- Windows PowerShell.
- Codex CLI installed and available as `codex` in `PATH`.
- A saved, active DeepSeek route in the DeepSeek card.

The Codex card selects a project directory, verifies the Codex CLI, starts the
Gateway when necessary, and opens Codex in a visible PowerShell window. The
selected directory is passed as the process working directory; it is not
embedded in a shell command string.

`npm run build` and `npm run build:web` validate and build the web frontend.
Use `npm run build:wails` before running the Wails build from `desktop-app/`.

The initial provider scope is intentionally limited to DeepSeek. The card
configures the DeepSeek V4 Pro or V4 Flash model and routes the fixed
`moonbridge` alias to the selected model through Moon Bridge's config graph API.

## Isolated Codex environment

Every Desktop-launched Codex process uses a Desktop-owned home:

```text
%APPDATA%\Moon Bridge Desktop\codex-home
```

The Go sidecar's `-print-codex-config moonbridge` command is the only source of
Codex configuration content. Desktop publishes its generated `config.toml`
atomically and leaves `models_catalog.json` and optional `auth.json` under the
same dedicated directory. The normal `%USERPROFILE%\.codex` directory is never
read, merged, or overwritten.

Codex always uses the last saved DeepSeek model and Reasoning values. Unsaved
changes in the DeepSeek card are shown separately and are not implicitly saved
when Codex is launched.

## Gateway and process lifetime

If Codex is launched while the Gateway is stopped, Desktop starts it and keeps
it running because the visible Codex terminal depends on the local Gateway. A
Gateway stop request after a Codex launch requires confirmation. Closing Desktop
also shows a dependency warning; confirming closes Desktop and its managed
Gateway, while the visible Codex terminal is not intentionally force-killed.

This MVP does not embed, supervise, resume, or stop Codex terminal sessions.
The terminal host may remain after Desktop exits, but its subsequent API calls
can fail when the Desktop-managed Gateway is no longer available.

## Scope and limitations

- Windows is the only supported Codex launch platform in this phase.
- The public model alias is fixed to `moonbridge`.
- Codex installation and authentication are not managed by Desktop.
- Multiple named Codex sessions, session history, and embedded terminals are
  outside the current scope.

## Stored API keys

API keys saved through the DeepSeek card are encrypted with Windows DPAPI for
the current Windows user before they are written to the SQLite store. The key
is only decrypted in memory when the Gateway builds a provider client; it is
never returned to the UI, API, logs, or errors. Because encryption is tied to
the current user, copying the database to another PC or another Windows user
will make stored keys unusable — re-enter the key (or use the
`DEEPSEEK_API_KEY` environment variable) after such a move. On non-Windows
platforms stored keys are not supported; use the environment variable instead.
