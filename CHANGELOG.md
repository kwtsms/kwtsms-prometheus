# Changelog

## [1.0.0] - Unreleased

### Added

- Alertmanager webhook receiver: one SMS per alert group through kwtSMS, recipients per receiver.
- Bearer token required; dedupe of retried notifications; MAX_PAGES, MAX_NUMBERS and MAX_SMS_PER_MINUTE guards.
- Permanent kwtSMS errors answered with HTTP 400 (no retry), transient ones with 500.
- Docker image (distroless, non-root).
