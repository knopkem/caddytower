# Copilot instructions for CaddyTower

CaddyTower is a public-facing VPS control plane. Treat every change as security-sensitive and optimize for correctness, reliability, and low resource usage.

## Engineering standards

- Prefer simple, explicit Go code over clever abstractions.
- Keep changes small and cohesive; avoid unrelated refactors.
- Preserve the lightweight design: no heavy background services, queues, or large dependencies unless clearly justified.
- Validate all user-controlled input at the boundary and return actionable errors.
- Do not swallow errors or use broad fallback behavior that can hide failed deployments, failed backups, or broken security controls.
- Keep secrets encrypted at rest. Never log credentials, tokens, TOTP secrets, connection strings, raw environment values, or backup contents.

## Security requirements

- Assume the admin UI can be exposed to the public internet behind Caddy.
- Require authenticated routes for all project, backup, log, settings, and adoption actions.
- Use CSRF protection for every state-changing browser form.
- Keep session cookies `HttpOnly`, `SameSite=Strict`, and `Secure` when the public URL is HTTPS.
- Enforce HTTPS for non-local public URLs and require a master key for non-local deployments.
- Keep webhook authentication HMAC-based and constant-time. Avoid adding unauthenticated project actions.
- Treat Docker socket access as privileged. Do not expose arbitrary exec, shell, or file browsing features through the UI.
- Make storage-heavy features opt-in and configurable.

## Testing expectations

- Add or update tests for every behavioral change.
- Cover security-sensitive paths: auth failures, CSRF rejection, invalid signatures, invalid config, path traversal, and disabled feature states.
- Run `CGO_ENABLED=0 go test ./...` and `CGO_ENABLED=0 go build ./...` before considering work complete.

## Documentation expectations

- Update `README.md` when behavior, configuration, deployment, security assumptions, or migration steps change.
- Add concise comments only where code is security-sensitive or non-obvious.
- Keep examples production-safe: do not suggest public HTTP admin URLs, plaintext secrets, or enabled backups without explaining storage impact.
