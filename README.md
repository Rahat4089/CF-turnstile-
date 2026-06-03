# CF Turnstile Solver

Go-based Cloudflare Turnstile solver with worker tabs, JSON task storage, and `.env`-driven configuration.

## Quick Start

```bash
git clone https://github.com/abh4xk/CF-turnstile-.git
cd CF-turnstile-
cp .env.example .env
make install
make run
```

## Configuration (`.env`)

All runtime variables are loaded from `.env` (with defaults when missing):

| Variable | Default | Description |
|---|---|---|
| `NUM_BROWSERS` | `2` | Number of Chromium instances |
| `TABS_PER_BROWSER` | `10` | Worker tabs per browser |
| `MAX_RETRIES` | `1` | Retry count per task |
| `TIMEOUT_SECONDS` | `30` | Solve timeout |
| `MAX_QUEUE_SIZE` | `3000` | Buffered task queue size |
| `RESULT_TTL_SECONDS` | `1800` | Task retention in JSON DB |
| `BROWSER_RECYCLE_AFTER` | `100` | Recycle worker context after solves |
| `API_KEY` | `TEST_API_KEY_12345` | API key check |
| `SERVER_HOST` | `0.0.0.0` | Bind host |
| `SERVER_PORT` | `5073` | Bind port |
| `TASK_DB_PATH` | `tasks.json` | JSON database path |
| `HEADLESS` | `false` | Run Chromium headless |
| `AUTH_TOKEN` | `` | Optional auth token for `/cloudflare` |

## API Usage

### `POST /cloudflare` (cf-bypass compatible + enhanced)

Supports both `turnstile` and `iuam` modes.

#### Turnstile mode

```bash
curl -X POST http://localhost:5073/cloudflare \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "turnstile",
    "domain": "https://app.dataimpulse.com/sign-in",
    "siteKey": "0x4AAAAAABkXqSagwd6aVDFz",
    "action": "login",
    "cData": "optional_data",
    "proxy": {
      "host": "127.0.0.1",
      "port": 8080,
      "username": "user",
      "password": "pass"
    }
  }'
```

**Response**
```json
{
  "code": 200,
  "token": "0.xxxxx",
  "headers": { "...": "..." },
  "cookies": [],
  "solveTime": 1.42,
  "elapsed": "1.46s"
}
```

#### IUAM mode

```bash
curl -X POST http://localhost:5073/cloudflare \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "iuam",
    "domain": "https://example.com",
    "ttl": 60000
  }'
```

**Response**
```json
{
  "code": 200,
  "cf_clearance": "...",
  "user_agent": "Mozilla/5.0 ...",
  "headers": { "...": "..." },
  "cookies": [],
  "solveTime": 2.18,
  "cached": false,
  "elapsed": "2.20s"
}
```

### `POST /createTask`

Optional proxy payload fields are supported (`proxyUrl`, `proxyURL`, or `proxy`).

```bash
curl -X POST http://localhost:5073/createTask \
  -H "Content-Type: application/json" \
  -d '{
    "clientKey": "TEST_API_KEY_12345",
    "task": {
      "type": "TurnstileTask",
      "websiteURL": "https://example.com",
      "websiteKey": "0x4AAAAAAABkUYJ2ABcZqyJ",
      "action": "login",
      "cdata": "optional_data",
      "proxyUrl": "http://user:pass@127.0.0.1:8080"
    }
  }'
```

### `POST /getTaskResult`

```bash
curl -X POST http://localhost:5073/getTaskResult \
  -H "Content-Type: application/json" \
  -d '{
    "clientKey": "TEST_API_KEY_12345",
    "taskId": 407533072
  }'
```

### Legacy endpoints

- `GET /turnstile?url=URL&sitekey=KEY&proxy=http://user:pass@host:port`
- `GET /result?id=TASK_ID`

## Build

```bash
make build       # Native binary
make build-exe   # Windows .exe
```

The Windows `.exe` uses the same `.env` variables at runtime.
