<<<<<<< HEAD
# Financial Tracker (Go + htmx + Supabase)

Single-page app: Go serves HTML fragments, htmx (CDN) swaps them into `#root`. No third-party Go dependencies.

## Supabase table

The app reads your existing table. It expects these columns (rename in `main.go` → `Tx` if yours differ):

| column | type |
|---|---|
| id | bigint / uuid (primary key) |
| date | date |
| type | text (`Salary` = income, everything else = expense) |
| amount | numeric (stored positive; absolute value is used) |
| note | text |

Set `SUPABASE_TABLE` if the table isn't called `transactions`.
Use the **service_role** key (server-side only, never in the browser) or add RLS policies for the anon key.

## Run locally

```bash
cp .env.example .env   # fill in values
export $(grep -v '^#' .env | xargs)
go run .
```

## Deploy on Render

1. Push this folder to GitHub.
2. Render → New → Blueprint (uses `render.yaml`) or New Web Service: runtime **Go**, build `go build -o app .`, start `./app`.
3. Set env vars: `SUPABASE_URL`, `SUPABASE_KEY`, `APP_PIN` (`SESSION_SECRET` is generated).

## Keeping the service awake

- **Built in:** a goroutine requests `$RENDER_EXTERNAL_URL/healthz?deep=1` every 10 minutes (Render sets that variable automatically). The deep check also runs a 1-row Supabase query so the free Supabase project isn't paused for inactivity.
- **Recommended backup:** a self-ping cannot wake a service that is already asleep (after a deploy or restart, for example). Add a free external monitor (UptimeRobot, cron-job.org, or Better Stack) hitting `https://YOUR-APP.onrender.com/healthz` every 5–10 minutes.

Render's free tier gives 750 instance-hours per month, which covers one always-on service.
=======
# Gofinance
This is for test only 
>>>>>>> 7a3902c3559be38f85e285c5d2fa0b9f48677499
