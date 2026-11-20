# Implementation Specification for #629

## Title: feat(obs): ship an operator dashboard suite

### Changes Delivered
- Formal validation logic.
- Comprehensive regression testing.

### Example

Start the stack, then confirm the dashboard and its metrics endpoint answer
before opening the suite:

```sh
docker compose up -d
curl -fsS http://localhost:8080/health
curl -fsS http://localhost:8080/metrics | grep '^sorobeacon_polls_total'
# Then sign in at http://localhost:8080/login with API_TOKEN and open the suite.
```
