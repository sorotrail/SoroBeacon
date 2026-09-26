- closes #282
- closes #284
- closes #285
- closes #286

### Changes Made:
- **Alerts**: Implemented alert lifecycle management (acknowledgement & resolution) and flapping detection to damp flapping rules.
- **Notify**: Built a durable delivery queue that survives restart and added per-monitor message templating support.
