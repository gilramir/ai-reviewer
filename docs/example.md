# Retry policy

The system SHALL retry indefinitely until the operation succeeds. Operators have
reported that this occasionally saturates the upstream service during an
outage, though **no incident** has yet been attributed to it directly.

## Backoff

Retries are spaced by a fixed interval of one second.

| setting | value | notes |
|---------|------:|-------|
| interval | 1s | fixed |
| ceiling | none | see above |

- [x] Interval is configurable
- [ ] Ceiling is configurable

> Select any passage above and comment on it. Ask "why this?" to get an answer,
> or say "reword this" to have the document edited under you.
