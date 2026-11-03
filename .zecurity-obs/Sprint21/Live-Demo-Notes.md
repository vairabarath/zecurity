# Sprint 21 — Live Demo Notes (browser + login recovery)

## 1. Login not working — root cause and fix

**Problem.** The first attempt used `~/browser-agent.js`, a puppeteer-core script that launched Chrome with `--no-sandbox` and navigated to `https://accounts.google.com`. Puppeteer drives Chrome through the DevTools protocol, which sets automation flags (`--enable-automation`, `navigator.webdriver`, etc.). Google's sign-in page detects these and blocks login with "This browser or app may not be secure", so the user could not sign in. The Chrome process also exited shortly after, leaving no window.

**Fix.** Launched the real Chrome binary directly (not via puppeteer) as a normal browser instance:

```bash
nohup /home/inkyank-01/chrome/linux-155.0.8059.84/chrome-linux64/chrome \
  --no-sandbox --disable-setuid-sandbox \
  --user-data-dir=/tmp/chrome-profile \
  https://accounts.google.com > /tmp/chrome.log 2>&1 &
```

Because this Chrome is not controlled by puppeteer, Google treated it as a regular browser and the user signed in successfully. The `--user-data-dir=/tmp/chrome-profile` kept the session/profile isolated and reusable for later steps.

## 2. Live demo — how the browser was driven while visible

After login, the provider console dev server was started (`npm run dev` in `provider-console/`, port 5174) and the controller was running (8080/9090). A second Chrome window was opened to `http://localhost:5174` in the same profile.

For the automated UI walkthrough, puppeteer was used in **headed** mode (`headless: false`) so every action was visible on screen:

```js
const browser = await launch({
  executablePath: '/home/inkyank-01/chrome/linux-155.0.8059.84/chrome-linux64/chrome',
  headless: false,
  args: ['--no-sandbox', '--disable-setuid-sandbox'],
});
```

Steps performed in the visible browser:
1. Navigated to `http://localhost:5174` (login page) — screenshot `ui-01-login-page.png`.
2. Typed the super-admin email/password and clicked "Sign in".
3. Waited for navigation to Home, captured `ui-02-home.png` (email, role, session countdown visible).
4. Clicked each nav section in turn — Relays, Tenants, Audit, Certificates, Provider users — waiting ~1.5s and capturing a full-page screenshot after each (`ui-03-relays.png`, `ui-03-tenants.png`, `ui-03-audit.png`, `ui-03-certificates.png`, `ui-03-provider-users.png`).
5. Opened the account menu and clicked "Sign out", captured `ui-04-after-signout.png` (redirect back to login with notice).

Because `headless: false` was set, the Chrome window stayed on screen and the user watched the tabs/sections switch in real time. Screenshots were saved to `/home/inkyank-01/ui-*.png` for the record.

## 3. Why puppeteer could drive the console but not Google login

- Google sign-in specifically blocks automated/DevTools-controlled browsers (anti-automation checks), so puppeteer could not be used for the Google step.
- The provider console is a normal React SPA with no such anti-automation checks, so puppeteer could fill the form, click buttons, and navigate sections reliably.
- Splitting the two (real Chrome for Google login, puppeteer for console automation) gave both a working login and a reproducible, screenshot-backed UI walkthrough.

## 4. Acceptance run 2026-10-17: tools used

Full write-up: `~/Desktop/sprint21-browser-automation-tools.md`. Results: `Acceptance-Run-Checklist.md`.

**Browser (headed, visible):**
- puppeteer-core 25.14.0 (`~/node_modules/puppeteer-core`) driving Chrome for Testing 155.0.8059.84 on `DISPLAY=:0`, with `headless: false`, `slowMo: 40` and a fresh `userDataDir` per run.
- Puppeteer locators with `::-p-text(...)` click buttons, Radix dropdown items, Select options and dialog buttons.
- `page.on('request')` captures network calls per session (AT-CORE-3, AT-C.7).
- `page.on('framenavigated')` records visited URLs (no secrets in URLs).
- `page.evaluate` reads local/sessionStorage.
- `page.screenshot` saves evidence to `~/sprint21-ui/01…17-*.png`.
- Script: `~/sprint21-ui/scripts/ui_run.js`.

**API and backend (no browser):**
- Python 3 stdlib `urllib`: `~/sprint21-ui/scripts/live_api.py`, 44 HTTP checks.
- curl + jq; `docker exec psql` (read-only, hash prefix only); `docker stop/start ztna_valkey` (AT-H.7); `openssl s_client` (AT-R.7); `grep -F -f` leak search over the controller log and audit details.
- `go test` with the CI env vars (175 DB-backed tests, 0 skipped); `go build` with env overrides (AT-H.1).
- `npm run lint/test/build` (374 console tests); `cargo test` (relay, shield, connector renewal).

**Rules followed:**
- The user's super-admin account was only used to sign in and out; all mutations ran on throwaway operators.
- No failed logins against real accounts, so the rate limiter was never tripped.
- The password was kept in a 0600 scratch file, never in scripts or notes.
