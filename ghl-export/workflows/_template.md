# Workflow: <name in GHL>

**Status in GHL:** active / inactive
**What starts the flow:** e.g. "sign-up via the 'Free community' form on /community"
**Tags/fields the flow sets:** e.g. tag `community`
**When the flow stops early:** e.g. "if the contact gets the tag `customer`"

## Steps

| # | Delay before the email | Subject line | File with content | Condition |
|---|---|---|---|---|
| 1 | immediately | Welcome to … | `01-welcome.html` | – |
| 2 | 1 day | … | `02-….html` | only if not `customer` |
| 3 | 2 days | … | `03-….html` | – |

## Screenshots

Put them in this folder (`screenshot-1.png` …).

## Contacts who are mid-flow at the time of migration

Choose one:
- [ ] They start over in the new flow
- [ ] They continue from the step they reached (the agent calculates it from their sign-up date)
- [ ] They should not receive any more emails from this flow
